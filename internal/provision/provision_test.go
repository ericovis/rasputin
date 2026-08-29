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
	cfg, err := config.Load("../../rasputin.yaml")
	if err != nil {
		t.Fatalf("loading the repo config: %v", err)
	}
	// The real public key may not exist on every machine that runs these
	// tests, so substitute a stand-in of the same shape.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519.pub")
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEYTESTKEYTESTKEYTESTKEYTEST test@example"
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Provision.AuthorizedKeys = keyPath

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
		"/usr/share/zoneinfo/America/Sao_Paulo",
		"en_US.UTF-8",
		"resize2fs",
		"sfdisk",
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
		"sfdisk --no-reread -N 2",
		"resize2fs",
		"rm -f \"$GROW_MARKER\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rasputin-identity is missing %q", want)
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
	files, err := Render(repoData(t))
	if err != nil {
		t.Fatal(err)
	}
	got := string(files[ProvisionServiceFile])
	for _, want := range []string{
		"After=network-online.target",
		"Wants=network-online.target",
		"apt-get update",
		"apt-get install -y",
		"podman curl htop vim git tmux",
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
	cfg, err := config.Load("../../rasputin.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provision.AuthorizedKeys = filepath.Join(t.TempDir(), "absent.pub")
	if _, err := NewData(cfg, "base.img"); err == nil {
		t.Error("NewData accepted a missing key file")
	}

	empty := filepath.Join(t.TempDir(), "empty.pub")
	if err := os.WriteFile(empty, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Provision.AuthorizedKeys = empty
	if _, err := NewData(cfg, "base.img"); err == nil {
		t.Error("NewData accepted an empty key file; nodes would be unreachable")
	}
}

func TestPackageList(t *testing.T) {
	d := Data{Packages: []string{"podman", "curl"}}
	if got := d.PackageList(); got != "podman curl" {
		t.Errorf("PackageList = %q", got)
	}
}

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

// fakeTools builds a bin dir of stub sfdisk/resize2fs/blockdev/partx that
// append their invocation (and stdin, for sfdisk) to a witness file.
// blockdev records itself too: it is the first tool the grow calls, so the
// no-marker test can detect an ungated grow even if later steps bail out.
// resize2fsExit lets one test simulate a failed online resize.
func fakeTools(t *testing.T, witness string, resize2fsExit int) string {
	t.Helper()
	bin := t.TempDir()
	stubs := map[string]string{
		"blockdev":  "echo \"blockdev $*\" >> \"$W\"\necho 32026656768\n",
		"sfdisk":    "in=$(cat)\necho \"sfdisk $* <<$in>>\" >> \"$W\"\n",
		"partx":     "echo \"partx $*\" >> \"$W\"\n",
		"partprobe": "echo \"partprobe $*\" >> \"$W\"\n",
		"resize2fs": fmt.Sprintf("echo \"resize2fs $*\" >> \"$W\"\nexit %d\n", resize2fsExit),
	}
	for name, body := range stubs {
		script := "#!/bin/sh\nW='" + witness + "'\n" + body
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// growSysDir writes the sysfs stand-ins for /sys/block/mmcblk0/mmcblk0p2:
// p2 starts at sector 1056768 and is currently smaller than the 32 GB card
// (32,026,656,768 B, as the cluster's cards report), so an attempted grow
// must reach sfdisk.
func growSysDir(t *testing.T, dir string) string {
	t.Helper()
	sys := filepath.Join(dir, "sys")
	if err := os.MkdirAll(sys, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "start"), []byte("1056768\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "size"), []byte("7331840\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return sys
}

// runIdentity executes the script with the grow inputs pointed at the sandbox.
func runIdentity(t *testing.T, script, bin, marker, sysDir string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("identity script execution is sandboxed only on darwin (no /sys/class/net/eth0)")
	}
	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RASPUTIN_GROW_MARKER="+marker,
		"RASPUTIN_GROW_DISK="+filepath.Join(filepath.Dir(marker), "disk"),
		"RASPUTIN_GROW_SYS="+sysDir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("identity exited non-zero: %v\n%s", err, out)
	}
}

func TestIdentityGrowIsGatedByTheMarker(t *testing.T) {
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness")
	bin := fakeTools(t, witness, 0)
	sys := growSysDir(t, dir)
	// No marker, but everything else primed for a grow: if the gate were
	// missing, blockdev (and then sfdisk) would write the witness.
	runIdentity(t, renderIdentity(t), bin, filepath.Join(dir, "absent-marker"), sys)
	if _, err := os.Stat(witness); !os.IsNotExist(err) {
		data, _ := os.ReadFile(witness)
		t.Errorf("without the marker the grow ran tools:\n%s", data)
	}
}

func TestIdentityGrowRunsOnceWithMarker(t *testing.T) {
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness")
	bin := fakeTools(t, witness, 0)
	marker := filepath.Join(dir, "grow-rootfs")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sys := growSysDir(t, dir)
	runIdentity(t, renderIdentity(t), bin, marker, sys)

	data, err := os.ReadFile(witness)
	if err != nil {
		t.Fatalf("the grow ran no tools: %v", err)
	}
	got := string(data)
	// want_sectors = 32026656768/512 - 1056768 = 61495296
	for _, want := range []string{"blockdev --getsize64", "sfdisk --no-reread -N 2", ",61495296", "resize2fs"} {
		if !strings.Contains(got, want) {
			t.Errorf("grow tool trace is missing %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("the marker survived a successful grow; the grow would re-run every boot")
	}
}

func TestIdentityGrowKeepsMarkerWhenResizeFails(t *testing.T) {
	dir := t.TempDir()
	witness := filepath.Join(dir, "witness")
	bin := fakeTools(t, witness, 1) // resize2fs fails
	marker := filepath.Join(dir, "grow-rootfs")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sys := growSysDir(t, dir)
	// Must still exit 0: the identity service may never block a boot.
	runIdentity(t, renderIdentity(t), bin, marker, sys)
	if _, err := os.Stat(marker); err != nil {
		t.Error("the marker was cleared even though resize2fs failed; the grow would never retry")
	}
}
