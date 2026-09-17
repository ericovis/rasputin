package agent

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/ericovis/rasputin/internal/kmsg"
)

// The writable layer.
//
// The golden rootfs (p2) is the overlay's lower layer and is mounted
// read-only for the life of the node: nothing ever writes to it again. Every
// change a running system makes lands in p3 instead, which is why a reset is
// seconds of deleting files rather than six minutes of rewriting a card. The
// node's identity survives it too, but not for free: the host keys are in the
// upper layer like everything else, and a reset carries them across by hand
// (see IdentityGlobs).
const (
	// UpperPart is the third partition. rasputin-identity appends it over
	// the free space of the card on a clone's first boot.
	UpperPart = "/dev/mmcblk0p3"
	// LowerMount and UpperMount are where the initramfs mounts p2 and p3
	// before combining them into the real root.
	LowerMount = "/lower"
	UpperMount = "/upper"
	// UpperSubdir and WorkSubdir are overlayfs's upperdir and workdir. Both
	// live inside p3 — overlayfs requires them on the same filesystem, and
	// refuses to use the root of one as either.
	UpperSubdir = "upper"
	WorkSubdir  = "work"
	// LayersDir is where the two layers are re-mounted inside the running
	// system, so that `df` on the node shows them. firstrun.sh creates it in
	// the lower layer; when it is missing the layers simply stay where the
	// initramfs put them.
	LayersDir = "/var/lib/rasputin/layers"
)

// UpperWait is how long HasUpper waits for p3's device node to appear. A node
// whose golden image predates the overlay has no p3 at all, so an absent one
// is a normal state rather than an error: this only covers the moment between
// the kernel finding the card and udev creating the node.
const UpperWait = 2 * time.Second

// upperPath and upperWait are what HasUpper actually looks at. They are
// variables so a test can point the lookup at a temp file and not wait.
var (
	upperPath = UpperPart
	upperWait = UpperWait
)

// procFilesystems lists the filesystems the kernel can mount. A variable for
// the same reason.
var procFilesystems = "/proc/filesystems"

const upperPollTick = 200 * time.Millisecond

// HasUpper reports whether this card has a writable layer.
func HasUpper() bool {
	deadline := time.Now().Add(upperWait)
	for {
		if _, err := os.Stat(upperPath); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(upperPollTick)
	}
}

// IdentityGlobs name the few files a reset carries over, relative to the
// upperdir.
//
// Everything else in the upper layer is exactly what a reset exists to throw
// away, but these are not "changes": seal strips the golden image of its host
// keys and machine-id precisely so that no two clones share them, and
// rasputin-identity mints them once, on first boot — into the upper layer,
// because the golden rootfs is read-only by then. Wiping them would give the
// node a new SSH identity on every reset, which breaks the pinned key in
// out/state.json, the operator's known_hosts and the DHCP lease the machine-id
// derives from. A reset is meant to undo what was done to a node, not to turn
// it into a different one.
var IdentityGlobs = []string{
	"etc/ssh/ssh_host_*",
	"etc/machine-id",
}

// WipeUpper empties a mounted writable layer: everything under its upperdir
// and workdir goes except the node's own identity (see IdentityGlobs), and
// both directories come back empty. That is the whole of a reset — with
// nothing left on top, the overlay shows the golden image again on the next
// mount.
func WipeUpper(dir string) error {
	upper := filepath.Join(dir, UpperSubdir)
	kept, err := readIdentity(upper)
	if err != nil {
		return err
	}
	for _, sub := range []string{UpperSubdir, WorkSubdir} {
		path := filepath.Join(dir, sub)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("emptying %s: %w", path, err)
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("recreating %s: %w", path, err)
		}
	}
	return writeIdentity(upper, kept)
}

// keptFile is one identity file held in memory across the wipe. They are a
// handful of kilobytes in total, so copying them out and back is simpler —
// and harder to get wrong — than deleting a tree around them.
type keptFile struct {
	rel  string
	mode os.FileMode
	data []byte
}

func readIdentity(upper string) ([]keptFile, error) {
	var kept []keptFile
	for _, glob := range IdentityGlobs {
		matches, err := filepath.Glob(filepath.Join(upper, glob))
		if err != nil {
			return nil, fmt.Errorf("looking for %s: %w", glob, err)
		}
		for _, path := range matches {
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", path, err)
			}
			rel, err := filepath.Rel(upper, path)
			if err != nil {
				return nil, err
			}
			kept = append(kept, keptFile{rel: rel, mode: info.Mode().Perm(), data: data})
		}
	}
	return kept, nil
}

func writeIdentity(upper string, kept []keptFile) error {
	for _, f := range kept {
		path := filepath.Join(upper, f.rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("recreating %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, f.data, f.mode); err != nil {
			return fmt.Errorf("restoring %s: %w", path, err)
		}
		// WriteFile only applies the mode when it creates the file, and a
		// private host key with the wrong mode is a host key sshd refuses.
		if err := os.Chmod(path, f.mode); err != nil {
			return fmt.Errorf("restoring the mode of %s: %w", path, err)
		}
	}
	return nil
}

// OverlayLoaded reports whether the kernel already knows the overlay
// filesystem, either built in or from a module loaded earlier.
func OverlayLoaded() bool {
	data, err := os.ReadFile(procFilesystems)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		for _, field := range strings.Fields(line) {
			if field == "overlay" || field == "overlayfs" {
				return true
			}
		}
	}
	return false
}

// EnsureOverlayModule makes sure the kernel can mount an overlay.
//
// The Raspberry Pi arm64 kernel builds overlayfs as a module and compresses
// modules with xz, and it has no in-kernel decompressor, so nothing but the
// initramfs can load it: read overlay.ko.xz out of the rootfs, unpack it here
// and hand the kernel the plain image. overlay has no module dependencies, so
// there is nothing else to resolve first.
//
// lowerRoot is the mounted golden rootfs and upperRoot the writable layer's
// upperdir; the writable layer is searched first, because an apt kernel
// upgrade installs its modules there and those are the ones matching the
// kernel that just booted.
func EnsureOverlayModule(lowerRoot, upperRoot, release string, log *kmsg.Logger) error {
	if OverlayLoaded() {
		return nil
	}
	path, image, err := readOverlayModule(lowerRoot, upperRoot, release, log)
	if err != nil {
		return err
	}
	logf(log, "overlay: loading %s (%d bytes unpacked)", path, len(image))
	return initModule(image)
}

// logf writes one line to the kernel log, tolerating the nil logger a test
// (or a boot with no /dev/kmsg) hands in.
func logf(log *kmsg.Logger, format string, args ...any) {
	if log != nil {
		log.Printf(format, args...)
	}
}

// moduleDirs are the two spellings of /lib/modules, both of which have to be
// tried under every root.
//
// Trixie is a merged-/usr system: dpkg installs a kernel's modules under
// /usr/lib/modules and /lib is only a symlink to usr/lib. The lower layer
// carries that symlink so either spelling resolves there, but the upper layer
// carries only what was written to it — a symlink nothing ever modified is
// never copied up — so an apt kernel upgrade lands under usr/lib/modules
// alone, and looking for lib/modules there finds nothing at all.
var moduleDirs = []string{"usr/lib/modules", "lib/modules"}

// OverlayModulePaths lists where the overlay module may be, in the order to
// try them: the writable layer before the golden rootfs, and the compressed
// module before the plain one, because that is what the Pi kernel ships.
func OverlayModulePaths(lowerRoot, upperRoot, release string) []string {
	var paths []string
	for _, root := range []string{upperRoot, lowerRoot} {
		if root == "" {
			continue
		}
		for _, mods := range moduleDirs {
			dir := filepath.Join(root, mods, release, "kernel", "fs", "overlayfs")
			paths = append(paths, filepath.Join(dir, "overlay.ko.xz"), filepath.Join(dir, "overlay.ko"))
		}
	}
	return paths
}

// readOverlayModule returns the first module image it finds, decompressed.
//
// A candidate that cannot be read or cannot be unpacked is not fatal: the
// writable layer comes first precisely because it may hold an independently
// produced copy, and a truncated one there — an apt run cut short by a power
// cut — must not hide the golden image's good copy behind it.
func readOverlayModule(lowerRoot, upperRoot, release string, log *kmsg.Logger) (string, []byte, error) {
	paths := OverlayModulePaths(lowerRoot, upperRoot, release)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if !strings.HasSuffix(path, ".xz") {
			return path, raw, nil
		}
		image, err := decompressXZ(raw)
		if err != nil {
			logf(log, "overlay: ignoring %s: unpacking it failed: %v", path, err)
			continue
		}
		return path, image, nil
	}
	return "", nil, fmt.Errorf("no overlay module for kernel %s in %s", release, strings.Join(paths, ", "))
}

// decompressXZ unpacks one xz stream in memory. The module is a couple of
// hundred kilobytes; there is nothing to stream.
func decompressXZ(raw []byte) ([]byte, error) {
	r, err := xz.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// OverlayOptions is the mount data that combines the two layers.
func OverlayOptions(lower, upper string) string {
	return fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		lower, filepath.Join(upper, UpperSubdir), filepath.Join(upper, WorkSubdir))
}
