// Package vanilla fetches the stock Raspberry Pi OS image.
//
// The download is ~1 GB of xz that expands to ~2.7 GB of image, so everything
// here streams: nothing is ever held in memory, and a completed download is
// cached so that repeated `prepare` runs cost nothing.
package vanilla

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

// ProgressInterval is how often a download reports itself.
const ProgressInterval = 64 << 20

// DefaultCacheDir and DefaultMetaPath are where a fetched image and its
// provenance live. Both are gitignored.
const (
	DefaultCacheDir = "cache"
	DefaultMetaPath = "out/meta/vanilla.json"
)

// Meta records exactly which image is in the cache. It is written next to the
// build outputs so that a golden image can always be traced back to the stock
// image it was baked from.
type Meta struct {
	ResolvedURL     string    `json:"resolved_url"`
	ImagePath       string    `json:"image_path"`
	SHA256XZ        string    `json:"sha256_xz"`
	SHA256Img       string    `json:"sha256_img"`
	Bytes           int64     `json:"bytes"`
	CompressedBytes int64     `json:"compressed_bytes"`
	FetchedAt       time.Time `json:"fetched_at"`
}

// Options configures Ensure.
type Options struct {
	// SourceURL is the download endpoint, typically a redirect to the
	// current release. Required.
	SourceURL string
	// CacheDir holds decoded images; defaults to DefaultCacheDir.
	CacheDir string
	// MetaPath is where vanilla.json is written; defaults to DefaultMetaPath.
	MetaPath string
	// HTTP defaults to a client with no overall timeout — this is a
	// gigabyte-scale download over a home connection.
	HTTP *http.Client
	// Log receives progress lines; nil discards them.
	Log func(format string, args ...any)
}

func (o *Options) applyDefaults() {
	if o.CacheDir == "" {
		o.CacheDir = DefaultCacheDir
	}
	if o.MetaPath == "" {
		o.MetaPath = DefaultMetaPath
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
}

// Ensure returns the path to a decoded stock image, downloading it only if
// the cache does not already hold it.
func Ensure(ctx context.Context, opts Options) (string, Meta, error) {
	opts.applyDefaults()
	if opts.SourceURL == "" {
		return "", Meta{}, fmt.Errorf("vanilla: SourceURL is required")
	}

	if meta, ok := cached(opts); ok {
		opts.Log("using the cached image %s (%d bytes)", meta.ImagePath, meta.Bytes)
		return meta.ImagePath, meta, nil
	}

	meta, err := download(ctx, opts)
	if err != nil {
		return "", Meta{}, err
	}
	if err := writeMeta(opts.MetaPath, meta); err != nil {
		return "", Meta{}, err
	}
	return meta.ImagePath, meta, nil
}

// cached reports whether the recorded image is still on disk at the recorded
// size. Size is checked rather than the hash: re-reading 2.7 GB on every
// prepare would cost more than it protects against, and a truncated file is
// the realistic failure.
func cached(opts Options) (Meta, bool) {
	meta, err := ReadMeta(opts.MetaPath)
	if err != nil {
		return Meta{}, false
	}
	if meta.ResolvedURL == "" || meta.ImagePath == "" {
		return Meta{}, false
	}
	info, err := os.Stat(meta.ImagePath)
	if err != nil || info.Size() != meta.Bytes || meta.Bytes == 0 {
		return Meta{}, false
	}
	return meta, true
}

func download(ctx context.Context, opts Options) (Meta, error) {
	opts.Log("fetching %s", opts.SourceURL)
	// The request carries the context, so an aborted `sync` stops a download
	// that is otherwise a gigabyte long: cancelling it fails the body read.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.SourceURL, nil)
	if err != nil {
		return Meta{}, fmt.Errorf("fetching %s: %w", opts.SourceURL, err)
	}
	resp, err := opts.HTTP.Do(req)
	if err != nil {
		return Meta{}, fmt.Errorf("fetching %s: %w", opts.SourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Meta{}, fmt.Errorf("fetching %s: HTTP %s", opts.SourceURL, resp.Status)
	}

	// The redirect target names the release, which is the only version
	// information the download endpoint gives us.
	resolved := opts.SourceURL
	if resp.Request != nil && resp.Request.URL != nil {
		resolved = resp.Request.URL.String()
	}
	imgPath := filepath.Join(opts.CacheDir, ImageName(resolved))
	opts.Log("resolved to %s", resolved)

	if err := os.MkdirAll(opts.CacheDir, 0o755); err != nil {
		return Meta{}, err
	}
	tmp, err := os.CreateTemp(opts.CacheDir, filepath.Base(imgPath)+".tmp-*")
	if err != nil {
		return Meta{}, err
	}
	tmpName := tmp.Name()
	// Any early return leaves no partial image behind to be mistaken for a
	// good one; the successful path renames the file out from under this.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	xzHash := sha256.New()
	imgHash := sha256.New()
	counted := &progressReader{
		r:        io.TeeReader(resp.Body, xzHash),
		interval: ProgressInterval,
		log:      opts.Log,
		what:     "downloaded",
		total:    resp.ContentLength,
	}

	xzr, err := xz.NewReader(counted)
	if err != nil {
		return Meta{}, fmt.Errorf("opening the xz stream: %w", err)
	}
	written, err := io.Copy(io.MultiWriter(tmp, imgHash), xzr)
	if err != nil {
		return Meta{}, fmt.Errorf("decoding %s after %d bytes: %w", resolved, written, err)
	}
	if err := tmp.Close(); err != nil {
		return Meta{}, err
	}
	if err := os.Rename(tmpName, imgPath); err != nil {
		return Meta{}, err
	}

	meta := Meta{
		ResolvedURL:     resolved,
		ImagePath:       imgPath,
		SHA256XZ:        hex.EncodeToString(xzHash.Sum(nil)),
		SHA256Img:       hex.EncodeToString(imgHash.Sum(nil)),
		Bytes:           written,
		CompressedBytes: counted.n,
		FetchedAt:       time.Now().UTC(),
	}
	opts.Log("decoded %d bytes to %s (xz %d bytes)", meta.Bytes, imgPath, meta.CompressedBytes)
	return meta, nil
}

// ImageName derives the cache file name from a download URL:
// ...-arm64-lite.img.xz becomes ...-arm64-lite.img.
func ImageName(rawURL string) string {
	p := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		p = u.Path
	}
	base := path.Base(p)
	base = strings.TrimSuffix(base, ".xz")
	if base == "" || base == "." || base == "/" {
		return "raspios.img"
	}
	if !strings.HasSuffix(base, ".img") {
		base += ".img"
	}
	return base
}

// ReadMeta loads a previously written vanilla.json.
func ReadMeta(path string) (Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return m, nil
}

func writeMeta(path string, m Meta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// progressReader logs every interval bytes so a long download shows life.
type progressReader struct {
	r        io.Reader
	n        int64
	last     int64
	interval int64
	total    int64
	what     string
	log      func(string, ...any)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.n-p.last >= p.interval {
		p.last = p.n
		if p.total > 0 {
			p.log("%s %d of %d MiB (%.0f%%)", p.what, p.n>>20, p.total>>20,
				100*float64(p.n)/float64(p.total))
		} else {
			p.log("%s %d MiB", p.what, p.n>>20)
		}
	}
	return n, err
}
