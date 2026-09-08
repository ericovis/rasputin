// Package prepare builds the customised Raspberry Pi OS image that every
// rasputin node starts from.
//
// The output is a stock image whose FAT boot partition has been given the
// recovery initramfs, the node identity table and the firstrun script. The
// ext4 root partition is untouched: everything that has to change inside it
// is changed on the Pi itself, on first boot.
package prepare

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/ericovis/rasputin/internal/bootfs"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/initramfs"
	"github.com/ericovis/rasputin/internal/provision"
	"github.com/ericovis/rasputin/internal/vanilla"
)

// Output paths, all under out/ and all gitignored.
const (
	OutDir       = "out"
	RecoveryPath = "out/recovery.gz"
	ImagePath    = "out/vanilla-custom.img"
	ImageZstPath = "out/vanilla-custom.img.zst"
	MetaPath     = "out/meta/prepare.json"
)

// SSHFlagFile is the empty marker that makes Raspberry Pi OS enable sshd on
// first boot. firstrun.sh needs a way in even before it has run.
const SSHFlagFile = "ssh"

// Options configures a prepare run.
type Options struct {
	// InitramfsOnly stops after building recovery.gz. The Makefile uses it,
	// and it is the fast path when only the agent has changed.
	InitramfsOnly bool
	// RemoveImage deletes the uncompressed out/vanilla-custom.img once it
	// has been compressed. It is 2.9 GB of regenerable intermediate and the
	// first thing to delete when the disk runs short, so `rasputin sync` sets
	// it; the plain `prepare` command leaves it in place because Day 0
	// writes that file to a card with dd.
	RemoveImage bool
	// Log receives progress lines; nil discards them.
	Log func(format string, args ...any)
}

// Meta records what one prepare run produced.
type Meta struct {
	BuildID      string    `json:"build_id"`
	BaseImage    string    `json:"base_image"`
	BaseURL      string    `json:"base_url"`
	ImagePath    string    `json:"image_path"`
	ImageBytes   int64     `json:"image_bytes"`
	ZstPath      string    `json:"zst_path"`
	ZstBytes     int64     `json:"zst_bytes"`
	SHA256Img    string    `json:"sha256_img"`
	SHA256Zst    string    `json:"sha256_zst"`
	RecoveryPath string    `json:"recovery_path"`
	RecoveryHash string    `json:"sha256_recovery"`
	PreparedAt   time.Time `json:"prepared_at"`
	// Fingerprint is what this build was made from: see Fingerprint. It is
	// how `rasputin sync` decides a prepare can be skipped.
	Fingerprint string `json:"fingerprint"`
}

// Run performs a full prepare and returns what it built.
//
// It takes a context because a cold prepare downloads ~500 MB and then moves
// ~3 GB twice: an aborted `rasputin sync` has to be able to stop it, and the
// long copies check the context as they stream.
func Run(ctx context.Context, cfg *config.Config, opts Options) (*Meta, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	logf := opts.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	logf("building the recovery initramfs")
	res, err := initramfs.Build(initramfs.Options{
		OutPath: RecoveryPath,
		Version: buildID(),
		Log:     logf,
	})
	if err != nil {
		return nil, err
	}
	recovery, err := os.ReadFile(res.OutPath)
	if err != nil {
		return nil, err
	}
	meta := &Meta{
		BuildID:      buildID(),
		RecoveryPath: res.OutPath,
		RecoveryHash: sha256hex(recovery),
		PreparedAt:   time.Now().UTC(),
	}
	if opts.InitramfsOnly {
		logf("initramfs only: stopping after %s (%d bytes)", res.OutPath, res.CompressedSize)
		return meta, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logf("ensuring the stock image is cached")
	basePath, baseMeta, err := vanilla.Ensure(ctx, vanilla.Options{
		SourceURL: cfg.Image.SourceURL,
		Log:       logf,
	})
	if err != nil {
		return nil, err
	}
	meta.BaseImage = filepath.Base(basePath)
	meta.BaseURL = baseMeta.ResolvedURL
	if meta.Fingerprint, err = Fingerprint(cfg, meta.BaseImage); err != nil {
		return nil, err
	}

	data, err := provision.NewData(cfg, meta.BaseImage)
	if err != nil {
		return nil, err
	}
	files, err := provision.Render(data)
	if err != nil {
		return nil, err
	}

	logf("copying %s to %s", basePath, ImagePath)
	if err := os.MkdirAll(OutDir, 0o755); err != nil {
		return nil, err
	}
	if err := copyFile(ctx, basePath, ImagePath); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logf("customising the boot partition")
	if err := customise(cfg, meta, recovery, files); err != nil {
		return nil, err
	}

	logf("compressing %s", ImagePath)
	if err := compress(ctx, meta, logf); err != nil {
		return nil, err
	}
	if opts.RemoveImage {
		if err := removeIntermediate(ImagePath, logf); err != nil {
			return nil, err
		}
	}
	if err := writeMeta(meta); err != nil {
		return nil, err
	}
	logf("prepared image %s (%d bytes, build %s)", meta.ZstPath, meta.ZstBytes, meta.BuildID)
	return meta, nil
}

// customise writes everything the node needs onto the FAT boot partition.
func customise(cfg *config.Config, meta *Meta, recovery []byte, files provision.Rendered) error {
	img, err := bootfs.Open(ImagePath)
	if err != nil {
		return err
	}
	defer img.Close()

	// The recovery initramfs, and the config.txt line that loads it.
	if err := img.WriteFile(bootfs.RecoveryFile, recovery); err != nil {
		return err
	}
	configTxt, err := img.ReadFile("config.txt")
	if err != nil {
		return err
	}
	if err := img.WriteFile("config.txt", bootfs.PatchConfigTxt(configTxt)); err != nil {
		return err
	}
	cmdline, err := img.ReadFile("cmdline.txt")
	if err != nil {
		return err
	}
	if err := img.WriteFile("cmdline.txt", bootfs.WithFirstrun(cmdline)); err != nil {
		return err
	}

	// Identity: which MAC is which node.
	pairs := make([][2]string, 0, len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		pairs = append(pairs, [2]string{n.MAC, n.Name})
	}
	if err := img.WriteFile(provision.NodesFile, bootfs.NodesConf(pairs)); err != nil {
		return err
	}

	for name, data := range files {
		if err := img.WriteFile(name, data); err != nil {
			return err
		}
	}
	if err := img.WriteFile(provision.BuildIDFile, []byte(meta.BuildID+"\n")); err != nil {
		return err
	}
	// Raspberry Pi OS enables sshd when it finds this marker.
	if err := img.WriteFile(SSHFlagFile, nil); err != nil {
		return err
	}
	return img.Close()
}

// compress writes the .zst beside the image and renames it into place.
//
// The rename is the point: prepare.json is only written when everything
// succeeded, so a run that dies mid-compress would otherwise leave a
// truncated out/vanilla-custom.img.zst next to the *previous* run's metadata,
// and `sync` would happily bake a builder from it.
func compress(ctx context.Context, meta *Meta, logf func(string, ...any)) error {
	in, err := os.Open(ImagePath)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	meta.ImagePath, meta.ImageBytes = ImagePath, info.Size()

	tmpPath := ImageZstPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		// Harmless once the rename has happened, and the whole point before it.
		os.Remove(tmpPath)
	}()

	imgHash, zstHash := sha256.New(), sha256.New()
	enc, err := zstd.NewWriter(io.MultiWriter(out, zstHash),
		zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithEncoderCRC(true))
	if err != nil {
		return err
	}
	n, err := io.Copy(enc, io.TeeReader(ctxReader{ctx, in}, imgHash))
	if err != nil {
		enc.Close()
		return fmt.Errorf("compressing after %d bytes: %w", n, err)
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	zstInfo, err := out.Stat()
	if err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, ImageZstPath); err != nil {
		return err
	}
	meta.ZstPath, meta.ZstBytes = ImageZstPath, zstInfo.Size()
	meta.SHA256Img = hex.EncodeToString(imgHash.Sum(nil))
	meta.SHA256Zst = hex.EncodeToString(zstHash.Sum(nil))
	logf("compressed %d bytes to %d (%.1f%%)", meta.ImageBytes, meta.ZstBytes,
		100*float64(meta.ZstBytes)/float64(meta.ImageBytes))
	return nil
}

// removeIntermediate deletes the uncompressed image once the .zst exists.
// A missing file is not a failure: the caller only wants it gone.
func removeIntermediate(path string, logf func(string, ...any)) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("removing the intermediate %s: %w", path, err)
	}
	logf("removed %s (regenerable intermediate)", path)
	return nil
}

func copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, ctxReader{ctx, in}); err != nil {
		return err
	}
	return out.Sync()
}

// ctxReader stops a multi-gigabyte copy at the next block once the run is
// aborted, instead of at the end of the file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// buildID is a sortable, unique-enough stamp: a UTC timestamp plus six
// random hex digits. It goes into the image, into /etc/rasputin-release on
// every node, and into prepare.json, so a running node can be traced back to
// the exact build it came from.
var currentBuildID string

func buildID() string {
	if currentBuildID == "" {
		var b [3]byte
		if _, err := rand.Read(b[:]); err != nil {
			// A predictable suffix is better than failing a build; the
			// timestamp alone is already nearly unique.
			b = [3]byte{0, 0, 0}
		}
		currentBuildID = time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
	}
	return currentBuildID
}

func writeMeta(m *Meta) error {
	if err := os.MkdirAll(filepath.Dir(MetaPath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(MetaPath, append(data, '\n'), 0o644)
}

// ReadMeta loads the record of the last prepare run.
func ReadMeta() (*Meta, error) { return readMetaAt(MetaPath) }

func readMetaAt(path string) (*Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &m, nil
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
