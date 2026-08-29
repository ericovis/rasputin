package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/ericovis/rasputin/internal/kmsg"
)

// Tunables for the download/write pipeline. The buffer is large because SD
// cards are slow and hate small writes; the progress interval is large
// because /dev/kmsg is a ring buffer we do not want to flood.
const (
	CopyBufferSize   = 4 << 20
	ProgressInterval = 256 << 20

	// Backoff bounds for pre-write retries. Nothing has been written to the
	// card while these run, so waiting is always the safe choice.
	BackoffMin = 2 * time.Second
	BackoffMax = 60 * time.Second

	// RetryDelay is the pause after a failed streaming attempt.
	RetryDelay = 5 * time.Second

	probeTimeout          = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second

	// MACHeader carries the node's identity on every request so the CLI can
	// attribute progress and captures without guessing from the source IP.
	MACHeader = "X-Rasputin-Mac"
)

// Backoff yields the pre-write retry delays: 2s, 4s, 8s ... capped at 60s.
type Backoff struct{ current time.Duration }

// Next returns the next delay and advances the sequence.
func (b *Backoff) Next() time.Duration {
	if b.current == 0 {
		b.current = BackoffMin
		return b.current
	}
	b.current *= 2
	if b.current > BackoffMax {
		b.current = BackoffMax
	}
	return b.current
}

// Reset returns the sequence to its start.
func (b *Backoff) Reset() { b.current = 0 }

// Target is a destination for a reflash: the SD card in production, a
// discard writer during a dryrun, a buffer in tests.
type Target interface {
	io.Writer
	Sync() error
}

// discardTarget is the dryrun destination: it counts bytes and drops them.
type discardTarget struct{}

func (discardTarget) Write(p []byte) (int, error) { return len(p), nil }
func (discardTarget) Sync() error                 { return nil }

// DiscardTarget is a Target that throws the decoded image away, used by the
// dryrun mode to exercise the whole pipeline without touching the card.
func DiscardTarget() Target { return discardTarget{} }

// Stats describes one completed stream.
type Stats struct {
	Bytes    int64
	Duration time.Duration
}

// Rate is the average throughput in bytes per second, or 0 for an
// instantaneous stream.
func (s Stats) Rate() float64 {
	if s.Duration <= 0 {
		return 0
	}
	return float64(s.Bytes) / s.Duration.Seconds()
}

func (s Stats) String() string {
	return fmt.Sprintf("%d bytes in %s (%.1f MB/s)",
		s.Bytes, s.Duration.Round(time.Second), s.Rate()/1e6)
}

// Client runs the network side of the recovery modes. Everything it needs
// from the outside world is a field, so the whole pipeline is testable
// against an httptest server on the build host.
type Client struct {
	URL  string
	MAC  string
	Log  *kmsg.Logger
	HTTP *http.Client

	// Sleep defaults to a context-aware time.Sleep; tests replace it to run
	// retry logic instantly.
	Sleep func(ctx context.Context, d time.Duration)
	// Now defaults to time.Now.
	Now func() time.Time
}

// NewClient returns a Client with production defaults: no overall timeout
// (an image takes minutes to stream) but a bounded wait for response
// headers, so a black-holed server is noticed rather than hung on.
func NewClient(imageURL, mac string, log *kmsg.Logger) *Client {
	return &Client{
		URL: imageURL,
		MAC: mac,
		Log: log,
		HTTP: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: responseHeaderTimeout,
				DisableCompression:    true, // the payload is already zstd
			},
		},
	}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(ctx, d)
		return
	}
	sleepCtx(ctx, d)
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log.Printf(format, args...)
	}
}

// request builds a request carrying the node's MAC both as a header and as a
// query parameter, so servers and proxies that drop one still see the other.
func (c *Client) request(ctx context.Context, method, rawURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, WithMAC(rawURL, c.MAC), body)
	if err != nil {
		return nil, err
	}
	if c.MAC != "" {
		req.Header.Set(MACHeader, c.MAC)
	}
	return req, nil
}

// WithMAC adds ?mac=... to rawURL, leaving an existing mac parameter alone.
func WithMAC(rawURL, mac string) string {
	if mac == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	if q.Get("mac") != "" {
		return rawURL
	}
	q.Set("mac", mac)
	u.RawQuery = q.Encode()
	return u.String()
}

// Probe checks that the image is actually being served before anything
// irreversible happens. HEAD is enough for our own server; some static
// servers reject it, so a 405 falls back to a GET whose body is closed
// immediately.
func (c *Client) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := c.request(ctx, http.MethodHead, c.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			return fmt.Errorf("probe %s: HTTP %s", c.URL, resp.Status)
		}
	}

	getReq, gerr := c.request(ctx, http.MethodGet, c.URL, nil)
	if gerr != nil {
		return gerr
	}
	getResp, gerr := c.HTTP.Do(getReq)
	if gerr != nil {
		if err != nil {
			return err
		}
		return gerr
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: HTTP %s", c.URL, getResp.Status)
	}
	return nil
}

// WaitProbe retries Probe until it succeeds. maxTries of 0 means forever,
// which is what a real reflash wants: the card is untouched, so a node that
// waits is a node that can still be rescued by starting the server.
func (c *Client) WaitProbe(ctx context.Context, maxTries int) error {
	var b Backoff
	for try := 1; ; try++ {
		err := c.Probe(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if maxTries > 0 && try >= maxTries {
			return fmt.Errorf("image server not reachable at %s after %d tries: %w", c.URL, try, err)
		}
		d := b.Next()
		c.logf("image server not reachable (%v), retry in %s (card untouched)", err, d)
		c.sleep(ctx, d)
	}
}

// Stream downloads the image, decodes it and writes it to dst. The zstd
// frame checksum is the integrity check: a truncated or corrupted download
// fails the decode rather than silently writing a broken card.
func (c *Client) Stream(ctx context.Context, dst Target) (Stats, error) {
	start := c.now()
	req, err := c.request(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return Stats{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Stats{}, fmt.Errorf("GET %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Stats{}, fmt.Errorf("GET %s: HTTP %s", c.URL, resp.Status)
	}

	dec, err := zstd.NewReader(resp.Body, zstd.IgnoreChecksum(false))
	if err != nil {
		return Stats{}, fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()

	n, err := c.copyWithProgress(ctx, dst, dec.IOReadCloser())
	stats := Stats{Bytes: n, Duration: c.now().Sub(start)}
	if err != nil {
		return stats, err
	}
	if err := dst.Sync(); err != nil {
		return stats, fmt.Errorf("sync after %d bytes: %w", n, err)
	}
	return stats, nil
}

// copyWithProgress is io.CopyBuffer with periodic logging and a periodic
// Sync, so a multi-minute write reports progress and does not build up an
// enormous dirty page cache on a 1 GB Pi.
func (c *Client) copyWithProgress(ctx context.Context, dst Target, src io.Reader) (int64, error) {
	buf := make([]byte, CopyBufferSize)
	var written, lastReport int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			written += int64(nw)
			if werr != nil {
				return written, fmt.Errorf("write at offset %d: %w", written, werr)
			}
			if nw != nr {
				return written, fmt.Errorf("short write at offset %d: %d of %d bytes", written, nw, nr)
			}
			if written-lastReport >= ProgressInterval {
				lastReport = written
				c.logf("written %d MiB", written>>20)
				if err := dst.Sync(); err != nil {
					return written, fmt.Errorf("sync at offset %d: %w", written, err)
				}
			}
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, fmt.Errorf("read at offset %d: %w", written, rerr)
		}
	}
}

// uploadConcurrency bounds the parallel zstd encode of a capture to three
// workers on the Pi 3's four A53 cores: the fourth core is left for the
// caller goroutine (SD read + xxhash CRC + memcpy into job buffers) and the
// HTTP flusher. The bound is also the RAM ceiling — see newUploadEncoder.
// NEVER pass 0 here: 0 means GOMAXPROCS, which grows the worst-case buffer
// count, and the agent is /init on a 1 GB Pi where an OOM is a kernel panic
// that needs a physical power cycle.
const uploadConcurrency = 3

// newUploadEncoder returns the encoder Upload streams a capture through.
//
// WithConcurrentBlocks switches the streaming path from one-block-at-a-time
// on a single core (measured 12.96 MB/s on an A53) to job-parallel encoding:
// 32 MiB jobs (4x the 8 MiB SpeedDefault window) with a 1 MiB overlap
// prefix, flushed in order as a normal single-frame zstd stream.
//
// RAM ceiling at concurrency 3 (klauspost/compress v1.19.2): job input
// buffers can exist in the filling slot (1), the job channel (3), the
// workers (3), the result channel (3) and the flusher (1) = 11 x 32 MiB
// = 352 MiB, plus ~64 MiB of in-flight compressed output, ~7 MiB of
// overlap prefixes and ~60 MiB of encoder window/table state: ~490 MiB
// absolute worst case, reached only if the network stalls completely (at
// which point dispatch blocks and, with all buffers pooled, allocation
// stops). Steady state is far lower (~200 MiB): the SD read at ~20-23 MB/s
// is slower than three workers' ~39 MB/s aggregate, so the queues run
// empty. Both fit a 1 GB Pi with >350 MiB headroom.
func newUploadEncoder(w io.Writer) (*zstd.Encoder, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(uploadConcurrency),
		zstd.WithConcurrentBlocks(true))
}

// Upload streams size bytes from src to the CLI as a zstd-compressed POST.
// This is the capture path: the card is only ever read, so failures are
// harmless and retried by the caller.
func (c *Client) Upload(ctx context.Context, src io.Reader, size int64) (Stats, error) {
	start := c.now()
	pr, pw := io.Pipe()
	counted := &countingReader{r: io.LimitReader(src, size)}

	go func() {
		enc, err := newUploadEncoder(pw)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		_, cerr := io.CopyBuffer(enc, counted, make([]byte, CopyBufferSize))
		if cerr != nil {
			enc.Close()
			pw.CloseWithError(cerr)
			return
		}
		if err := enc.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()

	req, err := c.request(ctx, http.MethodPost, c.URL, pr)
	if err != nil {
		pr.CloseWithError(err)
		return Stats{}, err
	}
	req.Header.Set("Content-Type", "application/zstd")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		pr.CloseWithError(err)
		return Stats{Bytes: counted.n, Duration: c.now().Sub(start)}, fmt.Errorf("POST %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	stats := Stats{Bytes: counted.n, Duration: c.now().Sub(start)}
	if resp.StatusCode != http.StatusOK {
		return stats, fmt.Errorf("POST %s: HTTP %s", c.URL, resp.Status)
	}
	if counted.n != size {
		return stats, fmt.Errorf("uploaded %d bytes, expected %d", counted.n, size)
	}
	return stats, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// DryrunReport is the summary written back to the boot partition, where an
// operator (and the CLI over SSH) can read it after the node reboots.
type DryrunReport struct {
	URL     string
	MAC     string
	Attempt int
	Stats   Stats
	Err     error
}

// Result is the one-line verdict, in the same shape the legacy shell
// prototype wrote, so existing eyes and greps still work.
func (r DryrunReport) Result() string {
	if r.Err != nil {
		if r.Attempt == 0 {
			return fmt.Sprintf("FAILED: %v", r.Err)
		}
		return fmt.Sprintf("FAILED attempt=%d: %v", r.Attempt, r.Err)
	}
	return fmt.Sprintf("OK attempt=%d %s", r.Attempt, r.Stats)
}

// String is the full file content written to reflash-dryrun.log.
func (r DryrunReport) String() string {
	return fmt.Sprintf("url: %s\nmac: %s\nresult: %s\n", r.URL, macOrUnknown(r.MAC), r.Result())
}

// sleepCtx waits for d, or until ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func macOrUnknown(mac string) string {
	if mac == "" {
		return "unknown"
	}
	return mac
}
