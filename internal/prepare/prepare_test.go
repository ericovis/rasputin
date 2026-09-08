package prepare

import (
	"bytes"
	"compress/gzip"
	"context"
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
		// `rasputin sync` prepares with RemoveImage, so this gate needs a
		// plain `prepare` to have run — it keeps the raw image for Day 0.
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

// fingerprintCfg parses a config whose authorized_keys points at a real file,
// since provision.NewData reads it.
func fingerprintCfg(t *testing.T, yaml string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519.pub")
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEYTESTKEYTESTKEYTESTKEY test@example"
	if err := os.WriteFile(keyPath, []byte(key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(strings.ReplaceAll(yaml, "KEYPATH", keyPath)), "test.yaml")
	if err != nil {
		t.Fatalf("parsing the test config: %v", err)
	}
	return cfg
}

const fingerprintYAML = `
cluster: test
image:
  source_url: https://example.invalid/raspios
  rootfs_size_gb: 4
ssh:
  key: KEYPATH
  users: [berry]
provision:
  user: berry
  authorized_keys: KEYPATH
  timezone: America/Sao_Paulo
  locale: en_US.UTF-8
  packages: [curl]
builder: rasputin001
nodes:
  - { name: rasputin001, mac: "b8:27:eb:00:00:01" }
  - { name: rasputin002, mac: "b8:27:eb:00:00:02" }
`

// TestFingerprint pins down prepare's idempotency key: it must be stable for
// an unchanged config (or `sync` would re-prepare on every run) and must move
// for anything that changes the bytes written to the boot partition.
func TestFingerprint(t *testing.T) {
	base := fingerprintCfg(t, fingerprintYAML)
	want, err := Fingerprint(base, "base.img")
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	again, err := Fingerprint(fingerprintCfg(t, fingerprintYAML), "base.img")
	if err != nil {
		t.Fatal(err)
	}
	if again != want {
		t.Errorf("Fingerprint is not stable: %s then %s (does it still hash the build-host clock?)", want, again)
	}

	cases := []struct {
		name string
		yaml string
		same bool
	}{
		{"a comment only", strings.Replace(fingerprintYAML, "cluster: test", "# a note for the next reader\ncluster: test", 1), true},
		{"a package added", strings.Replace(fingerprintYAML, "packages: [curl]", "packages: [curl, htop]", 1), false},
		{"the rootfs cap", strings.Replace(fingerprintYAML, "rootfs_size_gb: 4", "rootfs_size_gb: 8", 1), false},
		{"the provisioned user", strings.Replace(fingerprintYAML, "user: berry", "user: pi", 1), false},
		{"a node's mac", strings.Replace(fingerprintYAML, "b8:27:eb:00:00:02", "b8:27:eb:00:00:09", 1), false},
		{"a timeout", strings.Replace(fingerprintYAML, "builder: rasputin001", "timeouts:\n  bake_minutes: 60\nbuilder: rasputin001", 1), true},
		// The whole image is built on the stock one this URL names, and it
		// reaches none of the rendered files: without it in the key, pointing
		// the config at a new Raspberry Pi OS release would be a no-op run.
		{"the source url", strings.Replace(fingerprintYAML,
			"source_url: https://example.invalid/raspios",
			"source_url: https://example.invalid/raspios-trixie", 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Fingerprint(fingerprintCfg(t, tc.yaml), "base.img")
			if err != nil {
				t.Fatalf("Fingerprint: %v", err)
			}
			if tc.same && got != want {
				t.Errorf("changing %s changed the fingerprint (%s != %s); prepare would re-run for nothing", tc.name, got, want)
			}
			if !tc.same && got == want {
				t.Errorf("changing %s left the fingerprint at %s; prepare would be skipped and the image would be stale", tc.name, want)
			}
		})
	}
}

// TestFingerprintTracksTheBaseImage guards the other half of the key: a new
// stock image must invalidate a prepared one.
func TestFingerprintTracksTheBaseImage(t *testing.T) {
	cfg := fingerprintCfg(t, fingerprintYAML)
	a, err := Fingerprint(cfg, "2026-06-18-raspios.img")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(cfg, "2026-09-01-raspios.img")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("the fingerprint ignores the base image name")
	}
}

// TestCompressIsAtomic: prepare.json is only written once everything
// succeeded, so a compress that dies partway — an abort, or the out-of-disk
// case CLAUDE.md records — must not leave a truncated
// out/vanilla-custom.img.zst beside the *previous* run's metadata. `sync` reads
// only "the file is there" and the fingerprint, and would bake a builder
// from the short image.
func TestCompressIsAtomic(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	img := bytes.Repeat([]byte("raspios"), 1<<16)
	if err := os.WriteFile(ImagePath, img, 0o644); err != nil {
		t.Fatal(err)
	}
	good := []byte("the previous good build")
	if err := os.WriteFile(ImageZstPath, good, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := compress(ctx, &Meta{}, func(string, ...any) {}); err == nil {
		t.Fatal("compress reported success on an aborted run")
	}
	if got, err := os.ReadFile(ImageZstPath); err != nil || !bytes.Equal(got, good) {
		t.Errorf("the previous %s was overwritten by a failed compress (%d bytes, %v)",
			ImageZstPath, len(got), err)
	}
	if _, err := os.Stat(ImageZstPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a half-written temp file was left behind: %v", err)
	}

	meta := &Meta{}
	if err := compress(context.Background(), meta, func(string, ...any) {}); err != nil {
		t.Fatalf("compress: %v", err)
	}
	info, err := os.Stat(ImageZstPath)
	if err != nil {
		t.Fatalf("the compressed image: %v", err)
	}
	if info.Size() != meta.ZstBytes || meta.ZstBytes == 0 {
		t.Errorf("%s is %d bytes, meta says %d", ImageZstPath, info.Size(), meta.ZstBytes)
	}
}

func TestRemoveIntermediate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vanilla-custom.img")
	if err := os.WriteFile(path, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logged int
	logf := func(string, ...any) { logged++ }
	if err := removeIntermediate(path, logf); err != nil {
		t.Fatalf("removeIntermediate: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the intermediate is still there: %v", err)
	}
	if logged != 1 {
		t.Errorf("logged %d lines, want the deletion to be reported once", logged)
	}
	// Deleting twice must be quiet: `sync` sets RemoveImage on every prepare.
	if err := removeIntermediate(path, logf); err != nil {
		t.Errorf("removeIntermediate on a missing file: %v", err)
	}
}
