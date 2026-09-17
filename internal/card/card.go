// Package card writes a rasputin image to an SD card in this machine's own
// reader.
//
// It is the one write that does not go over the network, and the two cases
// that need it are the two the network cannot reach: day 0, when there is no
// node yet, and a node whose card died and has to be given a new one. The
// bytes and the checks are a flash's — the decode is agent.WriteImage, and
// the decoded length must be exactly what the image's partition table
// accounts for, the same rule bake applies to a capture — so a card written
// here and a card written by the recovery agent are the same card.
package card

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ericovis/rasputin/internal/agent"
	"github.com/ericovis/rasputin/internal/mbr"
)

// Device is a disk in this machine that an image could be written to.
type Device struct {
	// Path is what the operator and diskutil call it: /dev/disk4.
	Path string
	// RawPath is the unbuffered node the bytes actually go through.
	RawPath string
	Size    int64
	// Name is the reader's or the card's model name.
	Name      string
	Removable bool
}

// Report is one completed write.
type Report struct {
	// Device is the node that was written: the raw one for a disk.
	Device   string
	Bytes    int64
	Duration time.Duration
}

// Rate is the average throughput in bytes per second.
func (r Report) Rate() float64 {
	if r.Duration <= 0 {
		return 0
	}
	return float64(r.Bytes) / r.Duration.Seconds()
}

// ProgressInterval is how often Write reports. The agent's own interval is
// counted in bytes because it logs to a kernel ring buffer; here a person is
// watching a card that writes at 20 MB/s, and 256 MiB of silence reads as a
// hang.
const ProgressInterval = time.Second

// writeBlock is how much is batched before it reaches the disk. It matches
// the agent's copy buffer: SD cards are slow and hate small writes.
const writeBlock = agent.CopyBufferSize

// Write decodes the image at imagePath onto dev, which is either a disk
// (/dev/disk4, /dev/rdisk4) or a plain file to create. progress, when
// non-nil, is called at most every ProgressInterval with the bytes written so
// far and the total the image's partition table promises.
//
// A disk is unmounted first and ejected afterwards. Nothing reaches it until
// the first sector has been read and recognised as a partition table, so a
// file that is not an image never touches the card.
func Write(ctx context.Context, imagePath, dev string, progress func(written, total int64)) (Report, error) {
	src, err := os.Open(imagePath)
	if err != nil {
		return Report{}, err
	}
	defer src.Close()

	disk := IsDisk(dev)
	path := dev
	if disk {
		path = RawPath(dev)
		if err := unmount(BufferedPath(dev)); err != nil {
			return Report{}, err
		}
	}

	// O_TRUNC is for the file case — exporting an image over an older, longer
	// one must not leave its tail behind. A disk has nothing to truncate.
	flags := os.O_WRONLY | os.O_CREATE
	if !disk {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			// Writing a disk node needs root, and that is the single most
			// common way this command fails. Say the command that works.
			return Report{}, fmt.Errorf("opening %s: %v — run it again with sudo: sudo rasputin write-card -device %s",
				path, err, dev)
		}
		return Report{}, fmt.Errorf("opening %s: %w", path, err)
	}

	start := time.Now()
	raw := &aligned{w: f}
	dst := &checked{dst: raw, progress: progress}
	n, werr := agent.WriteImage(ctx, src, dst, nil)
	if werr == nil {
		// Every Sync along the way stopped at a sector boundary; the tail of
		// the image goes out here, once there is nothing left to append.
		werr = raw.finish()
	}
	cerr := f.Close()
	report := Report{Device: path, Bytes: n, Duration: time.Since(start)}

	switch {
	case werr != nil:
		return report, fmt.Errorf("writing %s to %s: %w", imagePath, path, werr)
	case cerr != nil:
		return report, fmt.Errorf("closing %s: %w", path, cerr)
	}
	if err := dst.verify(); err != nil {
		return report, fmt.Errorf("%s: %w", imagePath, err)
	}
	if err := readBack(path, dst.head); err != nil {
		return report, err
	}
	if disk {
		// Best effort: the card is written and correct whether or not the
		// operator has to pull it out themselves.
		eject(BufferedPath(dev))
	}
	return report, nil
}

// IsDisk reports whether dev names a disk rather than a file to create. Stat
// answers for anything that exists; the /dev/ prefix covers a node that does
// not, so a typo in a disk number is refused as a missing disk instead of
// quietly becoming a 4 GB file in /dev.
func IsDisk(dev string) bool {
	if info, err := os.Stat(dev); err == nil {
		return info.Mode()&(os.ModeDevice|os.ModeCharDevice) != 0
	}
	return strings.HasPrefix(dev, "/dev/")
}

// RawPath is the unbuffered node of a disk: /dev/disk4 becomes /dev/rdisk4.
// On macOS it is roughly an order of magnitude faster, and it is what the
// manual has always told operators to write to. Anything else is returned
// unchanged.
func RawPath(dev string) string {
	base := filepath.Base(dev)
	if filepath.Dir(dev) == "/dev" && strings.HasPrefix(base, "disk") {
		return "/dev/r" + base
	}
	return dev
}

// BufferedPath is the inverse of RawPath: /dev/rdisk4 becomes /dev/disk4.
// diskutil is given this one — it works on the disk, not on a node — while
// the bytes go to the raw one.
func BufferedPath(dev string) string {
	base := filepath.Base(dev)
	if filepath.Dir(dev) == "/dev" && strings.HasPrefix(base, "rdisk") {
		return "/dev/" + strings.TrimPrefix(base, "r")
	}
	return dev
}

// checked is the flash's verification, applied to the decoded bytes as they
// pass: the first sector must be a partition table with a boot and a root
// partition, and the stream must end exactly where that table says the used
// part of the card ends. The sector is held back until it parses, so the
// disk is not touched by something that is not an image.
type checked struct {
	dst   agent.Target
	table *mbr.Table
	head  []byte
	n     int64

	progress func(written, total int64)
	last     time.Time
}

func (c *checked) Write(p []byte) (int, error) {
	if c.table == nil {
		c.head = append(c.head, p...)
		c.n += int64(len(p))
		if len(c.head) < mbr.SectorSize {
			return len(p), nil
		}
		table, err := mbr.Parse(c.head[:mbr.SectorSize])
		if err != nil {
			return 0, err
		}
		if table.Partition(1) == nil || table.Partition(2) == nil {
			return 0, errors.New("the image has no boot and root partition pair; it is not a rasputin image")
		}
		c.table = table
		held := c.head
		// Keep a copy of the sector alone: it is what the read-back compares
		// against, and held is about to be handed on.
		c.head = append([]byte(nil), held[:mbr.SectorSize]...)
		if _, err := c.dst.Write(held); err != nil {
			return 0, err
		}
		c.report()
		return len(p), nil
	}
	if _, err := c.dst.Write(p); err != nil {
		return 0, err
	}
	c.n += int64(len(p))
	c.report()
	return len(p), nil
}

func (c *checked) Sync() error { return c.dst.Sync() }

// report rate-limits the progress callback to one call per interval.
func (c *checked) report() {
	if c.progress == nil {
		return
	}
	now := time.Now()
	if !c.last.IsZero() && now.Sub(c.last) < ProgressInterval {
		return
	}
	c.last = now
	c.progress(c.n, c.total())
}

func (c *checked) total() int64 {
	if c.table == nil {
		return 0
	}
	return c.table.UsedBytes()
}

// verify is the rule bake applies to a capture: an image whose length does
// not match its own partition table is truncated, padded or not an image.
func (c *checked) verify() error {
	if c.table == nil {
		return errors.New("the image is shorter than one sector; there is no partition table in it")
	}
	if want := c.total(); c.n != want {
		return fmt.Errorf("the image decodes to %d bytes but its partition table claims %d", c.n, want)
	}
	return nil
}

// aligned batches the decoded stream into whole sectors. A raw disk node on
// macOS refuses any write that is not a multiple of the sector size, and the
// zstd decoder hands out whatever the frame happens to hold. The agent needs
// none of this — it writes to /dev/mmcblk0, a block device that does not care
// — so the gluing lives here rather than in the shared pipeline.
type aligned struct {
	w   syncWriter
	buf []byte
}

// syncWriter is the file underneath, as an interface so a test can put a
// reader-of-sectors as strict as a raw disk node in its place.
type syncWriter interface {
	io.Writer
	Sync() error
}

func (a *aligned) Write(p []byte) (int, error) {
	a.buf = append(a.buf, p...)
	if len(a.buf) >= writeBlock {
		if err := a.flush(whole(len(a.buf))); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Sync puts the whole sectors it holds on the medium and keeps the rest.
//
// It is called mid-stream, every ProgressInterval bytes of the shared
// pipeline, and the buffer at those moments holds whatever the zstd decoder
// last handed over — a sector multiple only by chance. Flushing that would
// fail on a raw disk node, and, worse, leave every later write starting at a
// non-sector offset.
func (a *aligned) Sync() error {
	if err := a.flush(whole(len(a.buf))); err != nil {
		return err
	}
	return a.w.Sync()
}

// finish flushes the tail once the stream is over. An image is a whole number
// of sectors, so there is nothing partial left to refuse.
func (a *aligned) finish() error {
	if err := a.flush(len(a.buf)); err != nil {
		return err
	}
	return a.w.Sync()
}

// whole rounds down to a sector boundary.
func whole(n int) int { return n / mbr.SectorSize * mbr.SectorSize }

func (a *aligned) flush(n int) error {
	if n <= 0 {
		return nil
	}
	if _, err := a.w.Write(a.buf[:n]); err != nil {
		return err
	}
	a.buf = append(a.buf[:0], a.buf[n:]...)
	return nil
}

// readBack proves the medium took what was written. A card that is
// write-protected, or that has failed into a read-only state, accepts every
// write and keeps none of them; the first sector is where that shows up,
// and a card without its partition table boots nothing.
func readBack(path string, want []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading %s back: %w", path, err)
	}
	defer f.Close()

	got := make([]byte, len(want))
	if _, err := io.ReadFull(f, got); err != nil {
		return fmt.Errorf("reading %s back: %w", path, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s did not keep what was written to it: the first sector reads back different. "+
			"The card may be write-protected or failing", path)
	}
	return nil
}
