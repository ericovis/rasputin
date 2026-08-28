// Package initramfs builds recovery.gz: the gzipped newc archive the Pi's
// firmware loads on every boot of an adopted node.
//
// The archive is deliberately tiny — a single static aarch64 binary as /init
// plus a /dev/console device node — because it lives in RAM on a 1 GB Pi 3
// and because everything it cannot do, it cannot break.
package initramfs

import (
	"bytes"
	"compress/gzip"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/ericovis/rasputin/internal/cpio"
)

// AgentPackage is the package built into /init, relative to the repo root.
const AgentPackage = "./cmd/agent"

// AgentSizeWarn is the size above which the agent is worth a second look:
// the whole initramfs is copied into RAM before the kernel runs it.
const AgentSizeWarn = 12 << 20

// Options configures a build.
type Options struct {
	// RepoDir is the module root; `go build` runs there. Defaults to ".".
	RepoDir string
	// OutPath is where recovery.gz is written. Required.
	OutPath string
	// DefaultURL is baked into the agent as main.defaultURL, so that a bare
	// `touch /boot/firmware/reflash` works without the CLI. May be empty.
	DefaultURL string
	// Version is stamped as main.version for log correlation.
	Version string
	// Log receives human-readable progress; nil discards it.
	Log func(format string, args ...any)
}

// Result reports what a build produced.
type Result struct {
	OutPath        string
	AgentSize      int64 // uncompressed agent binary
	CompressedSize int64 // recovery.gz on disk
}

// Build cross-compiles the recovery agent for linux/arm64 and packs it into
// a gzipped newc archive at opts.OutPath.
//
// It shells out to the `go` toolchain, so it must run from a checkout of this
// repo with Go installed — which is always true, since this is a developer
// tool run from the repo root.
func Build(opts Options) (*Result, error) {
	if opts.OutPath == "" {
		return nil, fmt.Errorf("initramfs: OutPath is required")
	}
	repo := opts.RepoDir
	if repo == "" {
		repo = "."
	}
	logf := opts.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	tmp, err := os.MkdirTemp("", "rasputin-agent-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	agentPath := filepath.Join(tmp, "agent")

	logf("building %s for linux/arm64", AgentPackage)
	if err := buildAgent(repo, agentPath, opts); err != nil {
		return nil, err
	}
	agentBin, err := os.ReadFile(agentPath)
	if err != nil {
		return nil, err
	}
	if err := VerifyAgent(agentBin); err != nil {
		return nil, err
	}
	if len(agentBin) > AgentSizeWarn {
		logf("WARNING: agent is %.1f MiB, larger than the %d MiB budget for a RAM-resident initramfs",
			float64(len(agentBin))/(1<<20), AgentSizeWarn>>20)
	}

	archive, err := Pack(agentBin)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(opts.OutPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(opts.OutPath, archive, 0o644); err != nil {
		return nil, err
	}
	logf("wrote %s (%d bytes, agent %d bytes)", opts.OutPath, len(archive), len(agentBin))
	return &Result{
		OutPath:        opts.OutPath,
		AgentSize:      int64(len(agentBin)),
		CompressedSize: int64(len(archive)),
	}, nil
}

func buildAgent(repo, outPath string, opts Options) error {
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	ldflags := fmt.Sprintf("-s -w -X main.defaultURL=%s -X main.version=%s", opts.DefaultURL, version)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", outPath, AgentPackage)
	cmd.Dir = repo
	// CGO off is what makes the binary static; without it the agent would
	// need a dynamic loader that does not exist in the initramfs.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building the agent: %w\n%s", err, stderr.String())
	}
	return nil
}

// VerifyAgent checks that bin really is a static aarch64 executable. A
// dynamically linked or wrong-architecture /init produces a node that boots
// to nothing, so this is checked on every build rather than trusted.
func VerifyAgent(bin []byte) error {
	f, err := elf.NewFile(bytes.NewReader(bin))
	if err != nil {
		return fmt.Errorf("initramfs: agent is not an ELF binary: %w", err)
	}
	defer f.Close()
	if f.Machine != elf.EM_AARCH64 {
		return fmt.Errorf("initramfs: agent machine is %s, want EM_AARCH64", f.Machine)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("initramfs: agent is dynamically linked (has PT_INTERP); build with CGO_ENABLED=0")
		}
	}
	return nil
}

// Pack builds the gzipped newc archive around an agent binary.
func Pack(agentBin []byte) ([]byte, error) {
	var raw bytes.Buffer
	w := cpio.NewWriter(&raw)
	if err := w.WriteDir("dev"); err != nil {
		return nil, err
	}
	// /dev/console must exist before devtmpfs is mounted, or the kernel has
	// nowhere to send /init's early output.
	if err := w.WriteCharDev("dev/console", 0o600, 5, 1); err != nil {
		return nil, err
	}
	if err := w.WriteFile("init", 0o755, agentBin); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	gz, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gz.Write(raw.Bytes()); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
