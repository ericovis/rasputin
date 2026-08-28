package provision

import (
	"os"
	"os/exec"
	"path/filepath"
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
	files, err := Render(repoData(t))
	if err != nil {
		t.Fatal(err)
	}
	got := string(files[FirstrunFile])
	for _, want := range []string{
		"useradd -m -s /bin/bash \"$USER_NAME\"",
		"USER_NAME=berry",
		"ROOTFS_CAP_GB=8",
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
		"systemctl enable rasputin-provision.service",
		"/etc/rasputin-release",
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
	files, err := Render(repoData(t))
	if err != nil {
		t.Fatal(err)
	}
	got := string(files[SealFile])
	for _, want := range []string{
		"rm -f /etc/ssh/ssh_host_*",
		": > /etc/machine-id",
		"/var/lib/dbus/machine-id",
		"journalctl --vacuum-time=1s",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rasputin-seal is missing %q", want)
		}
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
