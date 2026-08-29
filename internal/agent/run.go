package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Disk abstracts the SD card so the mode runners are testable. In production
// these are backed by /dev/mmcblk0; in tests, by buffers.
type Disk interface {
	// OpenWrite opens the whole disk for writing.
	OpenWrite() (Target, error)
	// OpenRead opens the whole disk for reading, and reports how many bytes
	// at the front are actually in use according to the partition table.
	OpenRead() (io.ReadCloser, int64, error)
	// Close releases anything OpenWrite/OpenRead left open.
	Close() error
}

// System is the side of the world the mode runners cannot fake: rebooting
// and writing to the boot partition.
type System interface {
	// WithBoot mounts the boot partition read-write and runs fn on it.
	WithBoot(fn func(dir string) error) error
	// Reboot restarts the machine and does not return on success.
	Reboot() error
}

// Reflash overwrites the whole card from the image stream and reboots into
// it. It never gives up: the agent runs entirely from RAM, so a node that
// keeps retrying is always recoverable by fixing the server, while a node
// that gives up is a node someone has to walk to.
//
// The card is not opened for writing until a probe of the image URL has
// succeeded, so an unreachable server leaves the existing system intact.
func Reflash(ctx context.Context, c *Client, disk Disk) error {
	for attempt := 1; ; attempt++ {
		if err := c.WaitProbe(ctx, 0); err != nil {
			return err // only on context cancellation
		}
		c.logf("attempt %d: streaming %s -> disk", attempt, c.URL)

		stats, err := writeToDisk(ctx, c, disk)
		if err == nil {
			c.logf("flash OK: %s", stats)
			c.logf("rebooting into the fresh image")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.logf("attempt %d FAILED after %s: %v", attempt, stats, err)
		c.logf("the card may be partially written; retrying (recovery is RAM-resident)")
		c.sleep(ctx, RetryDelay)
	}
}

func writeToDisk(ctx context.Context, c *Client, disk Disk) (Stats, error) {
	target, err := disk.OpenWrite()
	if err != nil {
		return Stats{}, fmt.Errorf("opening the disk for writing: %w", err)
	}
	defer disk.Close()
	return c.Stream(ctx, target)
}

// DryrunAttempts is how many times a dryrun exercises the pipeline before
// giving up. It is bounded because a dryrun must always end in a reboot back
// into the normal system.
const DryrunAttempts = 3

// DryrunProbeTries bounds the probe retries per attempt, for the same reason.
const DryrunProbeTries = 5

// Dryrun runs the full download and decode pipeline into a discard writer,
// proving that a real reflash would work, and never touches the card.
func Dryrun(ctx context.Context, c *Client) DryrunReport {
	report := DryrunReport{URL: c.URL, MAC: c.MAC}
	for attempt := 1; attempt <= DryrunAttempts; attempt++ {
		report.Attempt = attempt
		if err := c.WaitProbe(ctx, DryrunProbeTries); err != nil {
			report.Err = err
			c.logf("dryrun %s", report.Result())
			if ctx.Err() != nil {
				return report
			}
			continue
		}
		stats, err := c.Stream(ctx, DiscardTarget())
		report.Stats, report.Err = stats, err
		if err == nil {
			return report
		}
		c.logf("dryrun %s", report.Result())
		if ctx.Err() != nil {
			return report
		}
		c.sleep(ctx, RetryDelay)
	}
	return report
}

// DryrunLog is the file a dryrun leaves on the boot partition.
const DryrunLog = "reflash-dryrun.log"

// Capture streams the used prefix of the card back to the CLI, which is how
// a golden image is made. Retried forever: the card is only read, so the
// only cost of waiting is time.
func Capture(ctx context.Context, c *Client, disk Disk) error {
	for attempt := 1; ; attempt++ {
		src, size, err := disk.OpenRead()
		if err != nil {
			return fmt.Errorf("opening the disk for reading: %w", err)
		}
		c.logf("attempt %d: capturing %d bytes (%d MiB) of used card -> %s",
			attempt, size, size>>20, c.URL)
		stats, err := c.Upload(ctx, src, size)
		src.Close()
		disk.Close()
		if err == nil {
			c.logf("capture OK: %s", stats)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.logf("capture attempt %d FAILED after %s: %v", attempt, stats, err)
		c.sleep(ctx, RetryDelay)
	}
}

// RunMode dispatches one of the network modes and performs the cleanup each
// one owes the next boot: clearing its flag file, leaving a report, and
// rebooting.
func RunMode(ctx context.Context, mode Mode, c *Client, disk Disk, sys System) error {
	switch mode {
	case ModeReflash:
		if err := Reflash(ctx, c, disk); err != nil {
			return err
		}
		// The reflash flag is not removed: it lived on the old card, which
		// no longer exists. The new image carries no flag.
		return sys.Reboot()

	case ModeDryrun:
		report := Dryrun(ctx, c)
		c.logf("dryrun result: %s", report.Result())
		if err := sys.WithBoot(func(dir string) error {
			return writeReportAndClearFlag(dir, report)
		}); err != nil {
			c.logf("WARNING: could not write the dryrun report: %v", err)
		}
		c.logf("dryrun complete, rebooting into the normal system")
		return sys.Reboot()

	case ModeCapture:
		// The flag must go BEFORE the card is read, not after: a capture
		// streams the boot partition along with everything else, so a flag
		// still on disk gets baked into the resulting image, and every node
		// later flashed from it wakes up believing it has been told to
		// capture. That is a self-replicating trap, so failing to clear the
		// flag is fatal — producing a poisoned image is worse than
		// producing none.
		if err := sys.WithBoot(func(dir string) error {
			return removeFlag(dir, FlagCapture)
		}); err != nil {
			return fmt.Errorf("refusing to capture: could not clear the capture flag first, "+
				"and capturing with it still on the card would poison the image: %w", err)
		}
		c.logf("capture flag cleared; reading the card")
		if err := Capture(ctx, c, disk); err != nil {
			return err
		}
		c.logf("capture complete, rebooting into the normal system")
		return sys.Reboot()
	}
	return fmt.Errorf("agent: mode %s is not a network mode", mode)
}

// writeReportAndClearFlag drops the dryrun report next to the flag file and
// then removes the flag, in that order: if the power fails between the two,
// the node retries the dryrun rather than losing the evidence.
func writeReportAndClearFlag(dir string, report DryrunReport) error {
	path := filepath.Join(dir, DryrunLog)
	if err := os.WriteFile(path, []byte(report.String()), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return removeFlag(dir, FlagDryrun)
}

// removeFlag deletes a flag file so the next boot is a normal one. An
// already-absent flag is success.
func removeFlag(dir, name string) error {
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the %s flag: %w", name, err)
	}
	return nil
}
