package provision

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ericovis/rasputin/internal/config"
)

func repoData(t *testing.T) Data {
	t.Helper()
	cfg, err := config.Load("../config/testdata/cluster.yaml")
	if err != nil {
		t.Fatalf("loading the fixture config: %v", err)
	}
	// The real public key may not exist on every machine that runs these
	// tests, so substitute a stand-in of the same shape.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519.pub")
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEYTESTKEYTESTKEYTESTKEYTEST test@example"
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Provision.AuthorizedKeys = config.KeySources{keyPath}

	d, err := NewData(cfg, "2026-06-18-raspios-trixie-arm64-lite.img")
	if err != nil {
		t.Fatalf("NewData: %v", err)
	}
	return d
}

func TestRenderProducesEveryFile(t *testing.T) {
	files, err := Render(repoData(t))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, name := range []string{FirstrunFile, IdentityFile, IdentityServiceFile, ProvisionServiceFile, SealFile} {
		if len(files[name]) == 0 {
			t.Errorf("%s was not rendered", name)
		}
	}
	if len(files) != 5 {
		t.Errorf("rendered %d files, want 5", len(files))
	}
}

func TestFirstrunContent(t *testing.T) {
	d := repoData(t)
	files, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	got := string(files[FirstrunFile])
	for _, want := range []string{
		"useradd -m -s /bin/bash \"$USER_NAME\"",
		"USER_NAME=berry",
		fmt.Sprintf("ROOTFS_CAP_GB=%d", d.RootfsSizeGB),
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5",
		"/etc/sudoers.d/010-rasputin",
		"visudo -c",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"/usr/share/zoneinfo/" + d.Timezone,
		"en_US.UTF-8",
		"resize2fs",
		"sfdisk",
		"/var/lib/rasputin/layers/lower /var/lib/rasputin/layers/upper",
		"systemctl enable rasputin-identity.service",
		"systemctl mask \"$unit\"",
		"userconfig.service",
		"systemctl enable rasputin-provision.service",
		"/etc/rasputin-release",
		"prepared_at=",
		"first_boot_at=",
		"2026-06-18-raspios-trixie-arm64-lite.img",
		"systemd\\.run=", // the disarming sed
		"BOOT=/boot/firmware",
		"rasputin-firstrun.log",
		"/dev/kmsg",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("firstrun.sh is missing %q", want)
		}
	}
	if strings.Contains(got, "{{") {
		t.Error("firstrun.sh still contains an unrendered template action")
	}
	// The template must not restart sshd: the boot ends in a reboot, and
	// restarting sshd mid-firstrun would drop the CLI's own session.
	if strings.Contains(got, "systemctl restart ssh") {
		t.Error("firstrun.sh restarts sshd; it should not")
	}
}

func TestIdentityContent(t *testing.T) {
	files, err := Render(repoData(t))
	if err != nil {
		t.Fatal(err)
	}
	got := string(files[IdentityFile])
	for _, want := range []string{
		"/sys/class/net/eth0/address",
		"BOOT=/boot/firmware",
		"NODES=$BOOT/nodes.conf",
		"/etc/hostname",
		"127.0.1.1",
		"ssh-keygen -A",
		"rasputin-unknown-",
		"exit 0",
		"GROW_MARKER=\"${RASPUTIN_GROW_MARKER:-/var/lib/rasputin/grow-rootfs}\"",
		"UPPER_PART=\"${RASPUTIN_UPPER_PART:-${GROW_DISK}p3}\"",
		"sfdisk --no-reread --append",
		"mkfs.ext4 -q -L rasputin-upper",
		"mkdir -p \"$LAYERS_DIR/lower\" \"$LAYERS_DIR/upper\"",
		"rm -f \"$GROW_MARKER\"",
		"$REBOOT_CMD",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rasputin-identity is missing %q", want)
		}
	}
	// The rootfs is the overlay's lower layer now and keeps the size it was
	// baked at; a clone that grew it to the whole card would leave no room
	// for the writable layer at all.
	for _, gone := range []string{"resize2fs", "sfdisk --no-reread -N 2"} {
		if strings.Contains(got, gone) {
			t.Errorf("rasputin-identity still grows the rootfs (%q)", gone)
		}
	}

	unit := string(files[IdentityServiceFile])
	for _, want := range []string{
		"DefaultDependencies=no",
		"After=local-fs.target",
		"Before=network-pre.target",
		"Type=oneshot",
		"WantedBy=sysinit.target",
		"/usr/local/sbin/rasputin-identity",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("rasputin-identity.service is missing %q", want)
		}
	}
}

func TestProvisionServiceContent(t *testing.T) {
	d := repoData(t)
	files, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	got := string(files[ProvisionServiceFile])
	// The package list comes from rasputin.yaml, so assert against what was
	// loaded rather than a copy of it that goes stale on the next edit.
	for _, want := range []string{
		"After=network-online.target",
		"Wants=network-online.target",
		"apt-get update",
		"apt-get install -y",
		d.PackageList(),
		"Restart=on-failure",
		"RestartSec=60",
		"/var/lib/rasputin/provisioned",
		"systemctl disable rasputin-provision.service",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rasputin-provision.service is missing %q", want)
		}
	}
}

func TestSealContent(t *testing.T) {
	d := repoData(t)
	files, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	got := string(files[SealFile])
	for _, want := range []string{
		"rm -f /etc/ssh/ssh_host_*",
		": > /etc/machine-id",
		"/var/lib/dbus/machine-id",
		"journalctl --vacuum-time=1s",
		"df -Pk /",
		"exit 1",
		": > /var/lib/rasputin/grow-rootfs",
		"dd if=/dev/zero of=/rasputin-zero bs=4M status=none",
		"rm -f /rasputin-zero",
		"skipping the zero fill",
		fmt.Sprintf("ROOTFS_CAP_GB=%d", d.RootfsSizeGB),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rasputin-seal is missing %q", want)
		}
	}
	// The fit guard exists to abort a bake before the builder is stripped, so
	// an abort leaves a fully provisioned, reachable node behind.
	if g, r := strings.Index(got, "df -Pk /"), strings.Index(got, "rm -f /etc/ssh/ssh_host_"); g < 0 || r < 0 || g > r {
		t.Error("the fit guard must run before seal strips anything (df guard not found before the host-key removal)")
	}
	// Removing the provisioning marker would make every clone re-run apt on
	// first boot, which the golden image exists precisely to avoid.
	if strings.Contains(got, "rm -f /var/lib/rasputin/provisioned") {
		t.Error("rasputin-seal clears the provisioning marker; clones would re-run apt")
	}
	for _, line := range strings.Split(got, "\n") {
		cmd := strings.TrimSpace(line)
		if strings.HasPrefix(cmd, "reboot") || strings.HasPrefix(cmd, "shutdown") {
			t.Errorf("rasputin-seal runs %q; the CLI orchestrates reboots", cmd)
		}
	}
}

// TestShellSyntax runs `sh -n` over every rendered script. It is the cheapest
// way to catch a template that produces something a node cannot run — and a
// node that cannot run firstrun.sh is a node that never comes back.
func TestShellSyntax(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this machine")
	}
	files, err := Render(repoData(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{FirstrunFile, IdentityFile, SealFile} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, files[name], 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(sh, "-n", path).CombinedOutput()
		if err != nil {
			t.Errorf("sh -n %s failed: %v\n%s", name, err, out)
		}
	}
}

func TestNewDataRejectsAMissingOrEmptyKey(t *testing.T) {
	cfg, err := config.Load("../config/testdata/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provision.AuthorizedKeys = config.KeySources{filepath.Join(t.TempDir(), "absent.pub")}
	if _, err := NewData(cfg, "base.img"); err == nil {
		t.Error("NewData accepted a missing key file")
	}

	empty := filepath.Join(t.TempDir(), "empty.pub")
	if err := os.WriteFile(empty, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Provision.AuthorizedKeys = config.KeySources{empty}
	if _, err := NewData(cfg, "base.img"); err == nil {
		t.Error("NewData accepted an empty key file; nodes would be unreachable")
	}
}

func TestNewDataJoinsFilesAndInlineKeys(t *testing.T) {
	cfg, err := config.Load("../config/testdata/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	const fileKey = "ssh-ed25519 AAAAFILE file@host"
	const inline = "ssh-ed25519 AAAAINLINE inline@host"
	one := filepath.Join(dir, "one.pub")
	if err := os.WriteFile(one, []byte(fileKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Provision.AuthorizedKeys = config.KeySources{one, inline}
	d, err := NewData(cfg, "base.img")
	if err != nil {
		t.Fatalf("NewData: %v", err)
	}
	if want := fileKey + "\n" + inline; d.AuthorizedKeys != want {
		t.Errorf("AuthorizedKeys = %q, want %q", d.AuthorizedKeys, want)
	}

	cfg.Provision.AuthorizedKeys = nil
	if _, err := NewData(cfg, "base.img"); err == nil {
		t.Error("NewData accepted an empty authorized_keys list; nodes would be unreachable")
	}
}

func TestPackageList(t *testing.T) {
	d := Data{Packages: []string{"podman", "curl"}}
	if got := d.PackageList(); got != "podman curl" {
		t.Errorf("PackageList = %q", got)
	}
}

// --- the one-shot writable layer ---------------------------------------
//
// rasputin-identity runs on every boot of every node, so the partitioning it
// does exactly once has to be gated, retried on failure and never able to
// block a boot. The tests below render the script and run it for real against
// stub tools, which is the only way to prove what it does with them.

// renderIdentity renders the identity script to an executable file.
func renderIdentity(t *testing.T) string {
	t.Helper()
	files, err := Render(repoData(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), IdentityFile)
	if err := os.WriteFile(path, files[IdentityFile], 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// upperSandbox is one rendered identity script plus everywhere it writes.
type upperSandbox struct {
	dir     string
	bin     string
	witness string
	marker  string
	part    string // stands in for /dev/mmcblk0p3
	fstype  string // what the blkid stub reports for part
	layers  string
	reboots string
}

// newUpperSandbox builds stub sfdisk/partx/blockdev/blkid/mkfs.ext4/reboot
// that append their invocation (and stdin, for sfdisk) to a witness file.
// sfdisk creates the partition node the way a real one does, so the script's
// wait for udev returns at once, and mkfs.ext4 records the filesystem blkid
// then reports — an unformatted partition is the state the script has to
// repair. mkfsExit lets a test simulate a failed mkfs.
func newUpperSandbox(t *testing.T, mkfsExit int) *upperSandbox {
	t.Helper()
	s := &upperSandbox{dir: t.TempDir()}
	s.bin = filepath.Join(s.dir, "bin")
	s.witness = filepath.Join(s.dir, "witness")
	s.marker = filepath.Join(s.dir, "grow-rootfs")
	s.part = filepath.Join(s.dir, "mmcblk0p3")
	s.fstype = filepath.Join(s.dir, "fstype")
	s.layers = filepath.Join(s.dir, "layers")
	s.reboots = filepath.Join(s.dir, "reboots")
	if err := os.MkdirAll(s.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	mkfs := fmt.Sprintf("echo \"mkfs.ext4 $*\" >> \"$W\"\nexit %d\n", mkfsExit)
	if mkfsExit == 0 {
		mkfs = "echo \"mkfs.ext4 $*\" >> \"$W\"\necho ext4 > '" + s.fstype + "'\n"
	}
	stubs := map[string]string{
		"blockdev":  "echo \"blockdev $*\" >> \"$W\"\necho 32026656768\n",
		"sfdisk":    "in=$(cat)\necho \"sfdisk $* <<$in>>\" >> \"$W\"\n: > '" + s.part + "'\n",
		"partx":     "echo \"partx $*\" >> \"$W\"\n",
		"partprobe": "echo \"partprobe $*\" >> \"$W\"\n",
		"blkid":     "echo \"blkid $*\" >> \"$W\"\ncat '" + s.fstype + "' 2>/dev/null || true\n",
		"mkfs.ext4": mkfs,
		"reboot":    "echo \"reboot $*\" >> '" + s.reboots + "'\n",
	}
	for name, body := range stubs {
		script := "#!/bin/sh\nW='" + s.witness + "'\n" + body
		if err := os.WriteFile(filepath.Join(s.bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *upperSandbox) armMarker(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(s.marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// makePart creates the partition node. fstype is what blkid will report for
// it: "" is a partition that was appended but never formatted.
func (s *upperSandbox) makePart(t *testing.T, fstype string) {
	t.Helper()
	if err := os.WriteFile(s.part, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if fstype == "" {
		return
	}
	if err := os.WriteFile(s.fstype, []byte(fstype+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run executes the rendered script with every path it touches pointed at the
// sandbox. It must always exit 0: the identity service may never block a boot.
func (s *upperSandbox) run(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("identity script execution is sandboxed only on darwin (no /sys/class/net/eth0)")
	}
	cmd := exec.Command("sh", renderIdentity(t))
	cmd.Env = append(os.Environ(),
		"PATH="+s.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RASPUTIN_GROW_MARKER="+s.marker,
		"RASPUTIN_GROW_DISK="+filepath.Join(s.dir, "disk"),
		"RASPUTIN_UPPER_PART="+s.part,
		"RASPUTIN_LAYERS_DIR="+s.layers,
		"RASPUTIN_REBOOT_CMD=reboot",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("identity exited non-zero: %v\n%s", err, out)
	}
}

func (s *upperSandbox) trace(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(s.witness)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestIdentityUpperIsGatedByTheMarker(t *testing.T) {
	s := newUpperSandbox(t, 0)
	// No marker, but everything else primed: an ungated run would partition
	// the card of a node that has been up for months.
	s.run(t)
	if got := s.trace(t); got != "" {
		t.Errorf("without the marker the identity script ran tools:\n%s", got)
	}
}

func TestIdentityUpperAppendsThePartitionOnceAndReboots(t *testing.T) {
	s := newUpperSandbox(t, 0)
	s.armMarker(t)
	s.run(t)

	got := s.trace(t)
	for _, want := range []string{
		"sfdisk --no-reread --append",
		"<<,,L>>", // one new partition over whatever free space is left
		// Only the new partition: partx -a on the whole disk fails because
		// p1 and p2 are registered already, and its failure would leave the
		// node waiting for a device node udev was never asked for.
		"partx -a --nr 3",
		"mkfs.ext4 -q -L rasputin-upper",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tool trace is missing %q:\n%s", want, got)
		}
	}
	for _, sub := range []string{"lower", "upper"} {
		if _, err := os.Stat(filepath.Join(s.layers, sub)); err != nil {
			t.Errorf("the %s mount point was not created: %v", sub, err)
		}
	}
	if _, err := os.Stat(s.marker); !os.IsNotExist(err) {
		t.Error("the marker survived; the node would repartition on every boot")
	}
	// The boot that creates the layer is running on the bare rootfs, so it
	// must not be allowed to continue on it.
	if _, err := os.Stat(s.reboots); err != nil {
		t.Error("the node did not reboot into the overlay after creating the writable layer")
	}
}

func TestIdentityUpperIsANoOpWhenTheLayerExists(t *testing.T) {
	s := newUpperSandbox(t, 0)
	s.armMarker(t)
	s.makePart(t, "ext4")
	s.run(t)

	if got := s.trace(t); strings.Contains(got, "sfdisk") {
		t.Errorf("an existing writable layer was repartitioned:\n%s", got)
	}
	if _, err := os.Stat(s.marker); !os.IsNotExist(err) {
		t.Error("the marker was kept although there is nothing left to do")
	}
	if _, err := os.Stat(s.reboots); err == nil {
		t.Error("a node with its layer already in place was rebooted for nothing")
	}
}

// TestIdentityUpperFormatsAPartitionLeftBehindByAFailedMkfs is the other half
// of the retry the marker promises: sfdisk has already run, so the partition
// node is there, but nothing was ever written to it. Taking the node's
// existence as proof of a finished layer would clear the marker and leave the
// node unable to mount its layer for the rest of its life.
func TestIdentityUpperFormatsAPartitionLeftBehindByAFailedMkfs(t *testing.T) {
	s := newUpperSandbox(t, 0)
	s.armMarker(t)
	s.makePart(t, "") // appended by an earlier boot, never formatted
	s.run(t)

	got := s.trace(t)
	if !strings.Contains(got, "mkfs.ext4 -q -L rasputin-upper") {
		t.Errorf("the empty partition was not formatted:\n%s", got)
	}
	if strings.Contains(got, "sfdisk") {
		t.Errorf("a second partition was appended over the first:\n%s", got)
	}
	if _, err := os.Stat(s.marker); !os.IsNotExist(err) {
		t.Error("the marker survived a repaired layer")
	}
	if _, err := os.Stat(s.reboots); err != nil {
		t.Error("the node did not reboot into the overlay after formatting the layer")
	}
}

func TestIdentityUpperKeepsTheMarkerWhenMkfsFails(t *testing.T) {
	s := newUpperSandbox(t, 1) // mkfs.ext4 fails
	s.armMarker(t)
	s.run(t)

	if _, err := os.Stat(s.marker); err != nil {
		t.Error("the marker was cleared even though mkfs failed; the layer would never be built")
	}
	if _, err := os.Stat(s.reboots); err == nil {
		t.Error("the node rebooted into an overlay that was never created")
	}
	// The failed mkfs left the partition node behind, and the next boot has
	// to try again rather than mistake it for a finished layer.
	if err := os.Remove(s.witness); err != nil {
		t.Fatal(err)
	}
	s.run(t)
	if got := s.trace(t); !strings.Contains(got, "mkfs.ext4") {
		t.Errorf("the next boot did not retry the mkfs:\n%s", got)
	}
}

// --- the seal script's zero fill ---------------------------------------------
//
// Whether seal writes a couple of gigabytes of zeros or none at all is one
// comparison, and every substring of that block survives inverting it or
// reading the wrong df column — so only running it proves anything. The whole
// script strips the machine it runs on, which is not a thing to do on a Mac,
// so the tests below slice out the block and drive it against stub tools.

// sealZeroFill runs the rendered seal script's zero-fill block with df
// reporting a rootfs of fsKB 1-KiB blocks (empty: df answers nothing at all).
// It returns what the block logged and whether dd was run.
func sealZeroFill(t *testing.T, fsKB string) (string, bool) {
	t.Helper()
	d := repoData(t)
	files, err := Render(d)
	if err != nil {
		t.Fatal(err)
	}
	seal := string(files[SealFile])
	from := strings.Index(seal, "fs_kb=$(df -Pk /")
	to := strings.Index(seal, "# Arm the one-shot writable layer")
	if from < 0 || to < 0 || to < from {
		t.Fatalf("cannot find the zero-fill block in the rendered seal script:\n%s", seal)
	}

	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	ddRan := filepath.Join(dir, "dd-ran")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		// A df -Pk row: field 2 is the filesystem's size, field 3 what it
		// uses. Reading the wrong one is exactly the mistake that would make
		// a mis-grown builder fill 30 GB with zeros, so they differ wildly.
		"df":   "[ -n \"${FS_KB:-}\" ] || exit 0\nprintf 'Filesystem 1024-blocks Used Available Capacity Mounted\\n/dev/root %s 12345 999 2%%%% /\\n' \"$FS_KB\"\n",
		"dd":   ": > '" + ddRan + "'\n",
		"du":   "printf '2048\\t%s\\n' \"$2\"\n",
		"sync": "",
		"rm":   "",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	script := fmt.Sprintf("#!/bin/sh\nset -u\nROOTFS_CAP_GB=%d\ncap_bytes=$((ROOTFS_CAP_GB * 1024 * 1024 * 1024))\n"+
		"log() { echo \"$*\" >> '%s'; }\n%s", d.RootfsSizeGB, logFile, seal[from:to])
	path := filepath.Join(dir, "zerofill.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", path)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FS_KB="+fsKB)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the zero-fill block exited non-zero: %v\n%s", err, out)
	}
	logged, err := os.ReadFile(logFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_, statErr := os.Stat(ddRan)
	return string(logged), statErr == nil
}

// TestSealZeroFillRunsOnACappedRootfs: the whole point of the change is that
// a builder at image.rootfs_size_gb writes at most cap-minus-used, which is a
// couple of GB and makes every later flash smaller.
func TestSealZeroFillRunsOnACappedRootfs(t *testing.T) {
	logged, ddRan := sealZeroFill(t, "4061000") // a 4 GiB rootfs, as firstrun leaves it
	if !ddRan {
		t.Errorf("the zero fill was skipped on a capped rootfs:\n%s", logged)
	}
	if !strings.Contains(logged, "filling the free space") {
		t.Errorf("log = %q, want the fill announced", logged)
	}
}

// TestSealZeroFillSkipsAnOvergrownRootfs is the guard's reason to exist: a
// builder whose rootfs was grown to the whole card would dd tens of gigabytes
// onto the flash for nothing.
func TestSealZeroFillSkipsAnOvergrownRootfs(t *testing.T) {
	logged, ddRan := sealZeroFill(t, "30000000") // grown to a 32 GB card
	if ddRan {
		t.Errorf("the zero fill wrote to a rootfs far past the cap:\n%s", logged)
	}
	if !strings.Contains(logged, "skipping the zero fill") {
		t.Errorf("log = %q, want the skip explained", logged)
	}
}

// TestSealZeroFillSkipsWhenTheSizeIsUnknown: an unmeasurable rootfs is not a
// reason to guess, and an empty fs_kb would make every comparison true.
func TestSealZeroFillSkipsWhenTheSizeIsUnknown(t *testing.T) {
	logged, ddRan := sealZeroFill(t, "")
	if ddRan {
		t.Errorf("the zero fill ran without knowing the size of /:\n%s", logged)
	}
	if !strings.Contains(logged, "skipping the zero fill") {
		t.Errorf("log = %q, want the skip explained", logged)
	}
}
