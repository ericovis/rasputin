package cluster

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/mbr"
	"github.com/ericovis/rasputin/internal/nodes"
	"github.com/ericovis/rasputin/internal/prepare"
	"github.com/ericovis/rasputin/internal/provision"
	"github.com/ericovis/rasputin/internal/server"
)

// Bake stages, in order.
const (
	provisionPoll  = 5 * time.Second
	captureTimeout = 45 * time.Minute
	sealScriptPath = "/usr/local/sbin/rasputin-seal"
)

// BakeResult describes a completed bake.
type BakeResult struct {
	Meta     *GoldenMeta
	Duration time.Duration
}

// Bake produces the golden image, using a real Pi as the arm64 builder.
//
// The sequence is: reflash the builder with the prepared stock image, let
// firstrun and the provisioning service turn it into a finished node, strip
// everything unique from it, then have the recovery agent stream the card
// back. What comes out is an image that every other node can be a
// byte-identical clone of.
func (c *Cluster) Bake(ctx context.Context, srv *server.Server) (*BakeResult, error) {
	start := time.Now()

	builder := c.Cfg.Node(c.Cfg.Builder)
	if builder == nil {
		return nil, fmt.Errorf("builder %q is not a configured node", c.Cfg.Builder)
	}
	prepMeta, err := prepare.ReadMeta()
	if err != nil {
		return nil, fmt.Errorf("no prepared image: run `rasputin prepare` first (%w)", err)
	}
	if !VanillaImage.Exists() {
		return nil, fmt.Errorf("%s is missing: run `rasputin prepare` first", VanillaImage.Path)
	}
	if err := srv.Register(VanillaImage.Name, VanillaImage.Path); err != nil {
		return nil, err
	}

	c.Log("baking on %s from %s (build %s)", builder.Name, prepMeta.BaseImage, prepMeta.BuildID)

	// 1. Reflash the builder with the prepared stock image.
	if err := c.bakeReflash(ctx, srv, *builder); err != nil {
		return nil, err
	}

	// 2. Wait for firstrun and provisioning to finish.
	conn, err := c.waitProvisioned(ctx, *builder)
	if err != nil {
		return nil, err
	}

	// 3. Strip everything that must be unique per node.
	c.Log("%s: sealing", builder.Name)
	if _, err := conn.Sudo(sealScriptPath); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%s: sealing: %w", builder.Name, err)
	}

	// 4. Capture the card.
	captureID := prepMeta.BuildID
	stagedPath := GoldenImage.Path + ".incoming"
	done, err := srv.ExpectCapture(captureID, stagedPath)
	if err != nil {
		conn.Close()
		return nil, err
	}
	captureURL := srv.CaptureURL(captureID)
	c.Log("%s: arming a capture to %s", builder.Name, captureURL)
	if err := WriteFlag(conn, FlagCapture, captureURL); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%s: writing the capture flag: %w", builder.Name, err)
	}

	if err := Reboot(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%s: cannot reboot for the capture: %w", builder.Name, err)
	}
	conn.Close()
	// Sealing deleted the builder's host keys, so it will generate new ones
	// on the way back. Nothing polls it over SSH during a capture, so this
	// is safe to drop now.
	if err := c.State.ForgetHostKey(builder.Name); err != nil {
		c.Log("%s: could not clear the recorded host key: %v", builder.Name, err)
	}

	if err := c.awaitCapture(ctx, *builder, srv, captureID, done); err != nil {
		return nil, err
	}
	result, _ := srv.CaptureResult(captureID)

	// 5. Verify the upload really decodes before anything is allowed to
	//    flash from it. A golden image that fails here would brick nodes.
	used, err := verifyImage(stagedPath)
	if err != nil {
		return nil, fmt.Errorf("the captured image is not usable: %w", err)
	}
	if err := os.Rename(stagedPath, GoldenImage.Path); err != nil {
		return nil, err
	}

	meta := &GoldenMeta{
		BuildID:  prepMeta.BuildID,
		SHA256:   result.SHA256,
		Bytes:    result.Bytes,
		Base:     prepMeta.BaseImage,
		Builder:  builder.Name,
		BakedAt:  time.Now().UTC(),
		CardUsed: used,
	}
	if err := WriteGoldenMeta(meta); err != nil {
		return nil, err
	}
	c.Log("golden image: %s (%d bytes compressed, %d bytes of card)", GoldenImage.Path, meta.Bytes, used)

	// 6. The builder reboots into its own sealed-then-healed system; make
	//    sure it actually comes back before calling the bake a success.
	c.Log("%s: waiting for the builder to come back", builder.Name)
	back, err := c.Resolver.WaitFor(ctx, *builder, time.Duration(c.Cfg.Timeouts.FlashMinutes)*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("the golden image was captured, but %w", err)
	}
	defer back.Close()
	var check FlashResult
	if err := c.verifyClone(back, *builder, meta, &check); err != nil {
		return nil, fmt.Errorf("the golden image was captured, but the builder came back wrong: %w", err)
	}
	c.Log("%s: healthy (%s, build %s)", builder.Name, check.Hostname, check.BuildID)
	// The builder was sealed and regenerated its host keys on the way back.
	c.PurgeKnownHosts(*builder, back.Host())

	return &BakeResult{Meta: meta, Duration: time.Since(start)}, nil
}

// bakeReflash writes the reflash flag pointing at the prepared stock image
// and waits for the builder to come back as a fresh install.
func (c *Cluster) bakeReflash(ctx context.Context, srv *server.Server, builder config.Node) error {
	conn, err := c.Resolver.Connect(ctx, builder)
	if err != nil {
		return err
	}
	url := srv.URLFor(VanillaImage.Name)
	c.Log("%s: reflashing with the prepared stock image from %s", builder.Name, url)
	if err := WriteFlag(conn, FlagReflash, url); err != nil {
		conn.Close()
		return fmt.Errorf("%s: writing the reflash flag: %w", builder.Name, err)
	}
	// firstrun.sh reboots once more after it finishes, so the first time the
	// node answers is not necessarily the last reboot.
	timeout := time.Duration(c.Cfg.Timeouts.BakeMinutes) * time.Minute
	back, err := c.rebootWatchingProgress(ctx, builder, conn, srv,
		RebootOptions{Back: timeout, ReplacesSystem: true})
	if err != nil {
		return err
	}
	back.Close()
	return nil
}

// waitProvisioned blocks until the provisioning service has installed the
// package set, surfacing its journal if it runs out of time.
func (c *Cluster) waitProvisioned(ctx context.Context, builder config.Node) (nodes.Conn, error) {
	deadline := time.Now().Add(time.Duration(c.Cfg.Timeouts.BakeMinutes) * time.Minute)
	c.Log("%s: waiting for provisioning to finish (up to %s)", builder.Name, time.Until(deadline).Round(time.Minute))

	for {
		conn, err := c.Resolver.Connect(ctx, builder)
		if err == nil {
			out, _ := conn.Output("test -f " + ProvisionedMarker + " && echo yes || echo no")
			if strings.TrimSpace(out) == "yes" {
				c.Log("%s: provisioned", builder.Name)
				return conn, nil
			}
			if time.Now().After(deadline) {
				journal, _ := conn.Output("sudo -n journalctl -u rasputin-provision.service --no-pager -n 40 || true")
				conn.Close()
				return nil, fmt.Errorf("%s: provisioning did not finish in time. Last log lines:\n%s",
					builder.Name, journal)
			}
			conn.Close()
		} else if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: provisioning did not finish in time and the node is unreachable: %w", builder.Name, err)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(provisionPoll):
		}
	}
}

// awaitCapture waits for the node's upload to complete, logging its size as
// it grows.
func (c *Cluster) awaitCapture(ctx context.Context, builder config.Node, srv *server.Server, id string, done <-chan struct{}) error {
	c.Log("%s: waiting for the card to stream back (up to %s)", builder.Name, captureTimeout)
	deadline := time.After(captureTimeout)
	ticker := time.NewTicker(ProgressTick)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("%s: the capture did not complete within %s", builder.Name, captureTimeout)
		case <-ticker.C:
			if info, err := os.Stat(srv.StagePath(id)); err == nil {
				c.Log("%s: captured %d MiB so far", builder.Name, info.Size()>>20)
			}
		}
	}
}

// verifyImage decodes a captured image end to end, checking the zstd frame
// checksums and the partition table, and returns the number of card bytes it
// contains.
func verifyImage(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	dec, err := zstd.NewReader(f, zstd.IgnoreChecksum(false))
	if err != nil {
		return 0, err
	}
	defer dec.Close()

	// The first sector must be a sane MBR, or this is not a disk image.
	head := make([]byte, mbr.SectorSize)
	if _, err := io.ReadFull(dec, head); err != nil {
		return 0, fmt.Errorf("reading the first sector: %w", err)
	}
	table, err := mbr.Parse(head)
	if err != nil {
		return 0, err
	}
	if table.Partition(1) == nil || table.Partition(2) == nil {
		return 0, fmt.Errorf("the captured image does not have both a boot and a root partition")
	}

	// Then decode the rest, which is what actually verifies the checksums.
	rest, err := io.Copy(io.Discard, dec)
	if err != nil {
		return 0, fmt.Errorf("decoding after %d bytes: %w", rest+mbr.SectorSize, err)
	}
	total := rest + mbr.SectorSize
	if want := table.UsedBytes(); total != want {
		return total, fmt.Errorf("the captured image is %d bytes but its partition table claims %d", total, want)
	}
	return total, nil
}

// SealFileName is the seal helper as installed on a node, exposed so tests
// and docs can refer to one definition.
const SealFileName = provision.SealFile
