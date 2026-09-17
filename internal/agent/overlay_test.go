package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// upperTree builds a mounted writable layer with something in both of its
// directories: files, a nested tree and a symlink, which is what a running
// system leaves behind.
func upperTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{UpperSubdir, WorkSubdir} {
		nested := filepath.Join(dir, sub, "etc", "ssh")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "sshd_config"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("sshd_config", filepath.Join(nested, "link")); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestWipeUpperEmptiesBothDirectories(t *testing.T) {
	dir := upperTree(t)
	// A real ext4 filesystem has one of these at its root; a reset must take
	// the two overlay directories and nothing else.
	if err := os.Mkdir(filepath.Join(dir, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := WipeUpper(dir); err != nil {
		t.Fatalf("WipeUpper: %v", err)
	}
	for _, sub := range []string{UpperSubdir, WorkSubdir} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			t.Fatalf("%s is gone after the wipe: %v", sub, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s still holds %d entries after the wipe", sub, len(entries))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "lost+found")); err != nil {
		t.Errorf("the wipe reached outside the overlay directories: %v", err)
	}
}

// TestWipeUpperKeepsTheNodesIdentity is the difference between a reset and a
// reflash. The golden image carries no host keys — seal removes them so no
// two clones share a fingerprint — so the ones the node minted on first boot
// live in the writable layer. Wiping them would hand the node a new SSH
// identity, and every pinned key that trusts it would (correctly) refuse the
// node afterwards.
func TestWipeUpperKeepsTheNodesIdentity(t *testing.T) {
	dir := upperTree(t)
	etc := filepath.Join(dir, UpperSubdir, "etc")
	if err := os.MkdirAll(filepath.Join(etc, "ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(etc, "ssh", "ssh_host_ed25519_key")
	if err := os.WriteFile(key, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(etc, "ssh", "ssh_host_ed25519_key.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	machineID := filepath.Join(etc, "machine-id")
	if err := os.WriteFile(machineID, []byte("deadbeef\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Written by the node's real work, and exactly what a reset throws away.
	junk := filepath.Join(etc, "ssh", "sshd_config.d")
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := WipeUpper(dir); err != nil {
		t.Fatalf("WipeUpper: %v", err)
	}
	for _, kept := range []struct {
		path string
		want string
		mode os.FileMode
	}{
		{key, "PRIVATE KEY", 0o600},
		{pub, "ssh-ed25519 AAAA", 0o644},
		{machineID, "deadbeef\n", 0o444},
	} {
		data, err := os.ReadFile(kept.path)
		if err != nil {
			t.Errorf("%s did not survive the reset: %v", kept.path, err)
			continue
		}
		if string(data) != kept.want {
			t.Errorf("%s = %q, want %q", kept.path, data, kept.want)
		}
		info, err := os.Stat(kept.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != kept.mode {
			t.Errorf("%s came back as %v, want %v — sshd refuses a key with the wrong mode",
				kept.path, info.Mode().Perm(), kept.mode)
		}
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Error("a directory that is not part of the node's identity survived the reset")
	}
}

func TestWipeUpperCreatesTheDirectoriesWhenTheyAreMissing(t *testing.T) {
	dir := t.TempDir()
	if err := WipeUpper(dir); err != nil {
		t.Fatalf("WipeUpper on a bare layer: %v", err)
	}
	for _, sub := range []string{UpperSubdir, WorkSubdir} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Errorf("%s was not created: %v", sub, err)
		}
	}
}

func TestHasUpperLooksForTheDeviceNode(t *testing.T) {
	dir := t.TempDir()
	old, oldWait := upperPath, upperWait
	t.Cleanup(func() { upperPath, upperWait = old, oldWait })

	upperWait = 0
	upperPath = filepath.Join(dir, "mmcblk0p3")
	if HasUpper() {
		t.Error("HasUpper found a writable layer that does not exist")
	}
	if err := os.WriteFile(upperPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasUpper() {
		t.Error("HasUpper missed an existing writable layer")
	}
}

// fakeModuleTree writes a module image under one of the two module
// directories of a root, compressing it when the name says so. mods is the
// spelling the layer really uses: a golden rootfs has /lib -> usr/lib, so
// either resolves there, while an upperdir only ever holds what dpkg wrote,
// which is usr/lib/modules.
func fakeModuleTree(t *testing.T, root, mods, release, name string, image []byte) {
	t.Helper()
	dir := filepath.Join(root, mods, release, "kernel", "fs", "overlayfs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := image
	if strings.HasSuffix(name, ".xz") {
		var buf bytes.Buffer
		w, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(image); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		body = buf.Bytes()
	}
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadOverlayModuleUnpacksTheCompressedModule(t *testing.T) {
	lower := t.TempDir()
	want := []byte("\x7fELF overlay module")
	fakeModuleTree(t, lower, "lib/modules", "6.12.0-rpi", "overlay.ko.xz", want)

	path, got, err := readOverlayModule(lower, filepath.Join(t.TempDir(), "upper"), "6.12.0-rpi", nil)
	if err != nil {
		t.Fatalf("readOverlayModule: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("module = %q, want the unpacked image %q", got, want)
	}
	if !strings.HasSuffix(path, "overlay.ko.xz") {
		t.Errorf("path = %q, want the compressed module", path)
	}
}

// TestReadOverlayModulePrefersTheWritableLayer: an apt kernel upgrade writes
// its modules to the upper layer, and those are the ones that match the
// kernel that booted. The golden image's copy is a version behind.
//
// The upper layer's tree is the one dpkg really produces — usr/lib/modules,
// with no /lib symlink, because nothing modified the golden's and so nothing
// copied it up — which is the only place this lookup can find it.
func TestReadOverlayModulePrefersTheWritableLayer(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	fakeModuleTree(t, lower, "lib/modules", "6.12.0-rpi", "overlay.ko.xz", []byte("from the golden image"))
	fakeModuleTree(t, upper, "usr/lib/modules", "6.12.0-rpi", "overlay.ko.xz", []byte("from the upgrade"))

	path, got, err := readOverlayModule(lower, upper, "6.12.0-rpi", nil)
	if err != nil {
		t.Fatalf("readOverlayModule: %v", err)
	}
	if string(got) != "from the upgrade" {
		t.Errorf("module = %q, want the writable layer's copy (%s)", got, path)
	}
}

// TestReadOverlayModuleSkipsACorruptCandidate: the writable layer is searched
// first, so a truncated overlay.ko.xz there — an apt run cut short by a power
// cut — must not shadow the good copy in the golden rootfs. Falling through is
// the difference between booting on the overlay and booting without it.
func TestReadOverlayModuleSkipsACorruptCandidate(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	fakeModuleTree(t, lower, "lib/modules", "6.12.0-rpi", "overlay.ko.xz", []byte("from the golden image"))
	fakeModuleTree(t, upper, "usr/lib/modules", "6.12.0-rpi", "overlay.ko.xz", nil)
	broken := filepath.Join(upper, "usr/lib/modules/6.12.0-rpi/kernel/fs/overlayfs/overlay.ko.xz")
	if err := os.WriteFile(broken, []byte("\xfd7zXZ truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, got, err := readOverlayModule(lower, upper, "6.12.0-rpi", nil)
	if err != nil {
		t.Fatalf("readOverlayModule: %v", err)
	}
	if string(got) != "from the golden image" {
		t.Errorf("module = %q (%s), want the golden image's copy", got, path)
	}
}

func TestReadOverlayModuleAcceptsAnUncompressedModule(t *testing.T) {
	lower := t.TempDir()
	fakeModuleTree(t, lower, "lib/modules", "6.12.0-rpi", "overlay.ko", []byte("plain module"))

	_, got, err := readOverlayModule(lower, "", "6.12.0-rpi", nil)
	if err != nil {
		t.Fatalf("readOverlayModule: %v", err)
	}
	if string(got) != "plain module" {
		t.Errorf("module = %q", got)
	}
}

func TestReadOverlayModuleNamesTheKernelItLookedFor(t *testing.T) {
	_, _, err := readOverlayModule(t.TempDir(), "", "6.12.0-rpi", nil)
	if err == nil {
		t.Fatal("readOverlayModule found a module in an empty rootfs")
	}
	for _, want := range []string{"6.12.0-rpi", "overlay.ko.xz"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestOverlayModulePathsOrder(t *testing.T) {
	got := OverlayModulePaths("/lower", "/upper/upper", "6.12.0-rpi")
	want := []string{
		"/upper/upper/usr/lib/modules/6.12.0-rpi/kernel/fs/overlayfs/overlay.ko.xz",
		"/upper/upper/usr/lib/modules/6.12.0-rpi/kernel/fs/overlayfs/overlay.ko",
		"/upper/upper/lib/modules/6.12.0-rpi/kernel/fs/overlayfs/overlay.ko.xz",
		"/upper/upper/lib/modules/6.12.0-rpi/kernel/fs/overlayfs/overlay.ko",
		"/lower/usr/lib/modules/6.12.0-rpi/kernel/fs/overlayfs/overlay.ko.xz",
		"/lower/usr/lib/modules/6.12.0-rpi/kernel/fs/overlayfs/overlay.ko",
		"/lower/lib/modules/6.12.0-rpi/kernel/fs/overlayfs/overlay.ko.xz",
		"/lower/lib/modules/6.12.0-rpi/kernel/fs/overlayfs/overlay.ko",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("paths =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestOverlayLoadedReadsProcFilesystems(t *testing.T) {
	old := procFilesystems
	t.Cleanup(func() { procFilesystems = old })

	dir := t.TempDir()
	procFilesystems = filepath.Join(dir, "filesystems")
	if err := os.WriteFile(procFilesystems, []byte("nodev\tsysfs\n\text4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if OverlayLoaded() {
		t.Error("OverlayLoaded reported an overlay the kernel does not know")
	}
	if err := os.WriteFile(procFilesystems, []byte("nodev\tsysfs\nnodev\toverlay\n\text4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !OverlayLoaded() {
		t.Error("OverlayLoaded missed the overlay filesystem")
	}
}

// TestEnsureOverlayModuleSkipsAnAlreadyLoadedOverlay covers the path a second
// boot takes, and is the only part of EnsureOverlayModule testable off Linux:
// loading a module is a syscall.
func TestEnsureOverlayModuleSkipsAnAlreadyLoadedOverlay(t *testing.T) {
	old := procFilesystems
	t.Cleanup(func() { procFilesystems = old })
	procFilesystems = filepath.Join(t.TempDir(), "filesystems")
	if err := os.WriteFile(procFilesystems, []byte("nodev\toverlay\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No module anywhere: if it tried to load one this would fail.
	if err := EnsureOverlayModule(t.TempDir(), "", "6.12.0-rpi", nil); err != nil {
		t.Errorf("EnsureOverlayModule: %v", err)
	}
}

func TestOverlayOptions(t *testing.T) {
	got := OverlayOptions(LowerMount, UpperMount)
	want := "lowerdir=/lower,upperdir=/upper/upper,workdir=/upper/work"
	if got != want {
		t.Errorf("OverlayOptions = %q, want %q", got, want)
	}
}
