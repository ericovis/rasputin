package prepare

import (
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericovis/rasputin/internal/bootfs"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/cpio"
	"github.com/ericovis/rasputin/internal/initramfs"
	"github.com/ericovis/rasputin/internal/provision"
)

// repoPath resolves a path relative to the repository root, since these
// tests inspect the artifacts a real `rasputin prepare` left in out/.
func repoPath(p string) string { return filepath.Join("../..", p) }

// TestImageContents is the no-hardware end-to-end gate: it opens the image
// that `rasputin prepare` produced and checks every property a node depends
// on to boot into the recovery agent and provision itself.
func TestImageContents(t *testing.T) {
	imgPath := repoPath(ImagePath)
	if _, err := os.Stat(imgPath); err != nil {
		t.Skipf("run `go run ./cmd/rasputin prepare` first: %v", err)
	}
	cfg, err := config.Load(repoPath("rasputin.yaml"))
	if err != nil {
		t.Fatalf("loading the config: %v", err)
	}

	img, err := bootfs.Open(imgPath)
	if err != nil {
		t.Fatalf("opening the prepared image: %v", err)
	}
	defer img.Close()

	t.Run("recovery initramfs", func(t *testing.T) {
		data, err := img.ReadFile(bootfs.RecoveryFile)
		if err != nil {
			t.Fatalf("recovery.gz: %v", err)
		}
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("recovery.gz is not gzip: %v", err)
		}
		defer gz.Close()
		entries, err := cpio.Read(gz)
		if err != nil {
			t.Fatalf("recovery.gz is not a newc archive: %v", err)
		}
		init := cpio.Find(entries, "init")
		if init == nil {
			t.Fatal("recovery.gz has no /init")
		}
		if err := initramfs.VerifyAgent(init.Data); err != nil {
			t.Errorf("/init: %v", err)
		}
		console := cpio.Find(entries, "dev/console")
		if console == nil || !console.IsCharDev || console.Major != 5 || console.Minor != 1 {
			t.Errorf("dev/console = %+v, want char 5:1", console)
		}
	})

	t.Run("nodes.conf", func(t *testing.T) {
		data, err := img.ReadFile(provision.NodesFile)
		if err != nil {
			t.Fatalf("nodes.conf: %v", err)
		}
		for _, n := range cfg.Nodes {
			line := n.MAC + "\t" + n.Name
			if !strings.Contains(string(data), line) {
				t.Errorf("nodes.conf is missing %q", line)
			}
		}
	})

	t.Run("config.txt", func(t *testing.T) {
		data, err := img.ReadFile("config.txt")
		if err != nil {
			t.Fatalf("config.txt: %v", err)
		}
		got := string(data)
		for _, want := range []string{bootfs.InitramfsLine, "dtoverlay=disable-wifi", "dtoverlay=disable-bt"} {
			if !strings.Contains(got, want) {
				t.Errorf("config.txt is missing %q", want)
			}
		}
		if strings.Contains(got, "auto_initramfs=") {
			t.Error("config.txt still sets auto_initramfs; the distribution initramfs would win")
		}
	})

	t.Run("cmdline.txt", func(t *testing.T) {
		data, err := img.ReadFile("cmdline.txt")
		if err != nil {
			t.Fatalf("cmdline.txt: %v", err)
		}
		got := strings.TrimSpace(string(data))
		if strings.Contains(got, "\n") {
			t.Error("cmdline.txt must be a single line")
		}
		for _, want := range []string{
			"cgroup_enable=memory", "cgroup_memory=1",
			"systemd.run=" + bootfs.FirstrunPath,
			"systemd.run_success_action=reboot",
			"systemd.unit=kernel-command-line.target",
			"root=PARTUUID=",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("cmdline.txt is missing %q\ngot: %s", want, got)
			}
		}
		for _, tok := range strings.Fields(got) {
			if strings.HasPrefix(tok, "init=") {
				t.Errorf("cmdline.txt still has the first-boot hook %q", tok)
			}
			if tok == "resize" {
				t.Error("cmdline.txt still has the `resize` token; captures would be 32 GB")
			}
		}
	})

	t.Run("provisioning files", func(t *testing.T) {
		for _, name := range []string{
			provision.FirstrunFile, provision.IdentityFile, provision.IdentityServiceFile,
			provision.ProvisionServiceFile, provision.SealFile, provision.BuildIDFile,
		} {
			if !img.Exists(name) {
				t.Errorf("%s is missing from the boot partition", name)
			}
		}
		if !img.Exists(SSHFlagFile) {
			t.Error("the ssh marker is missing; sshd would not start on first boot")
		}

		// firstrun.sh is the one file a node cannot survive being wrong.
		sh, err := exec.LookPath("sh")
		if err != nil {
			t.Skip("no sh on this machine")
		}
		data, err := img.ReadFile(provision.FirstrunFile)
		if err != nil {
			t.Fatalf("firstrun.sh: %v", err)
		}
		path := filepath.Join(t.TempDir(), "firstrun.sh")
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
			t.Errorf("firstrun.sh has a syntax error: %v\n%s", err, out)
		}
		if !strings.Contains(string(data), cfg.Provision.User) {
			t.Errorf("firstrun.sh does not mention the provisioned user %q", cfg.Provision.User)
		}
	})

	t.Run("build id matches the recorded meta", func(t *testing.T) {
		meta, err := readMetaAt(repoPath(MetaPath))
		if err != nil {
			t.Skipf("no prepare.json: %v", err)
		}
		data, err := img.ReadFile(provision.BuildIDFile)
		if err != nil {
			t.Fatalf("rasputin-build-id: %v", err)
		}
		if strings.TrimSpace(string(data)) != meta.BuildID {
			t.Errorf("image build id %q != prepare.json %q", strings.TrimSpace(string(data)), meta.BuildID)
		}
	})
}

func TestBuildIDIsStableWithinARun(t *testing.T) {
	currentBuildID = ""
	first := buildID()
	if first != buildID() {
		t.Error("buildID changed between calls in the same run")
	}
	if len(first) != len("20060102T150405Z")+7 {
		t.Errorf("buildID = %q, want a timestamp plus six hex digits", first)
	}
}

func TestReadMetaMissing(t *testing.T) {
	if _, err := readMetaAt(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("readMetaAt accepted a missing file")
	}
}
