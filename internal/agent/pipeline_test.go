package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestBackoffSequence(t *testing.T) {
	var b Backoff
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if got := b.Next(); got != w*time.Second {
			t.Fatalf("step %d = %s, want %s", i, got, w*time.Second)
		}
	}
	b.Reset()
	if got := b.Next(); got != BackoffMin {
		t.Errorf("after Reset = %s, want %s", got, BackoffMin)
	}
}

func TestWithMAC(t *testing.T) {
	cases := []struct{ in, mac, want string }{
		{"http://h/x.zst", "aa:bb", "http://h/x.zst?mac=aa%3Abb"},
		{"http://h/x.zst?mac=cc", "aa:bb", "http://h/x.zst?mac=cc"},
		{"http://h/capture?node=one", "aa", "http://h/capture?mac=aa&node=one"},
		{"http://h/x.zst", "", "http://h/x.zst"},
		{"://bad", "aa", "://bad"},
	}
	for _, tc := range cases {
		if got := WithMAC(tc.in, tc.mac); got != tc.want {
			t.Errorf("WithMAC(%q, %q) = %q, want %q", tc.in, tc.mac, got, tc.want)
		}
	}
}

// zstdOf compresses payload the way the CLI serves images.
func zstdOf(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testClient returns a Client pointed at url whose sleeps are instant.
func testClient(url string) *Client {
	c := NewClient(url, "b8:27:eb:01:02:03", nil)
	c.Sleep = func(context.Context, time.Duration) {}
	return c
}

type bufTarget struct {
	bytes.Buffer
	syncs int
}

func (b *bufTarget) Sync() error { b.syncs++; return nil }

func TestStreamDecodesAndCountsBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("rasputin"), 1000)
	image := zstdOf(t, payload)
	var gotMAC string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMAC = r.Header.Get(MACHeader)
		w.Write(image)
	}))
	defer srv.Close()

	var dst bufTarget
	stats, err := testClient(srv.URL+"/golden.img.zst").Stream(context.Background(), &dst)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Errorf("decoded %d bytes, want the original %d", dst.Len(), len(payload))
	}
	if stats.Bytes != int64(len(payload)) {
		t.Errorf("stats.Bytes = %d, want %d", stats.Bytes, len(payload))
	}
	if dst.syncs == 0 {
		t.Error("Stream did not Sync the target")
	}
	if gotMAC != "b8:27:eb:01:02:03" {
		t.Errorf("server saw MAC header %q", gotMAC)
	}
}

func TestStreamRejectsCorruptedImage(t *testing.T) {
	image := zstdOf(t, bytes.Repeat([]byte("x"), 100000))
	image[len(image)-3] ^= 0xff // corrupt the frame checksum
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(image)
	}))
	defer srv.Close()

	var dst bufTarget
	if _, err := testClient(srv.URL).Stream(context.Background(), &dst); err == nil {
		t.Fatal("a corrupted stream was accepted; the checksum is not being verified")
	}
}

func TestStreamRejectsTruncatedImage(t *testing.T) {
	image := zstdOf(t, bytes.Repeat([]byte("y"), 200000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(image[:len(image)/2])
	}))
	defer srv.Close()

	var dst bufTarget
	if _, err := testClient(srv.URL).Stream(context.Background(), &dst); err == nil {
		t.Fatal("a truncated stream was accepted")
	}
}

func TestStreamReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := testClient(srv.URL).Stream(context.Background(), &bufTarget{})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want a 404", err)
	}
}

func TestProbeHeadThenGetFallback(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Write([]byte("body"))
	}))
	defer srv.Close()

	if err := testClient(srv.URL).Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(methods) != 2 || methods[0] != http.MethodHead || methods[1] != http.MethodGet {
		t.Errorf("methods = %v, want HEAD then GET", methods)
	}
}

func TestProbeFailsOnNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	if err := testClient(srv.URL).Probe(context.Background()); err == nil {
		t.Fatal("Probe accepted a 404")
	}
}

func TestWaitProbeRetriesThenSucceeds(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			http.Error(w, "not yet", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := testClient(srv.URL).WaitProbe(context.Background(), 0); err != nil {
		t.Fatalf("WaitProbe: %v", err)
	}
	if hits < 3 {
		t.Errorf("server saw %d probes, want at least 3", hits)
	}
}

func TestWaitProbeGivesUpAfterMaxTries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	err := testClient(srv.URL).WaitProbe(context.Background(), 3)
	if err == nil || !strings.Contains(err.Error(), "after 3 tries") {
		t.Fatalf("err = %v, want a give-up after 3 tries", err)
	}
}

func TestWaitProbeHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := testClient("http://127.0.0.1:1/nothing")
	if err := c.WaitProbe(ctx, 0); err == nil {
		t.Fatal("WaitProbe ignored a cancelled context")
	}
}

func TestUploadPostsZstdOfExactlySize(t *testing.T) {
	card := bytes.Repeat([]byte("DISK"), 50000) // 200000 bytes "on the card"
	const used = 120000                         // ...of which only this is in use

	var received []byte
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		dec, err := zstd.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer dec.Close()
		received, err = io.ReadAll(dec)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stats, err := testClient(srv.URL+"/capture").Upload(context.Background(), bytes.NewReader(card), used)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if stats.Bytes != used {
		t.Errorf("stats.Bytes = %d, want %d", stats.Bytes, used)
	}
	if !bytes.Equal(received, card[:used]) {
		t.Errorf("server received %d bytes, want the first %d of the card", len(received), used)
	}
	if !strings.Contains(query, "mac=") {
		t.Errorf("capture URL query = %q, want a mac parameter", query)
	}
}

func TestUploadFailsOnShortDisk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	_, err := testClient(srv.URL).Upload(context.Background(), bytes.NewReader([]byte("short")), 1000)
	if err == nil || !strings.Contains(err.Error(), "uploaded 5 bytes") {
		t.Fatalf("err = %v, want a short-read complaint", err)
	}
}

func TestUploadFailsOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		http.Error(w, "no room", http.StatusInsufficientStorage)
	}))
	defer srv.Close()
	_, err := testClient(srv.URL).Upload(context.Background(), bytes.NewReader(make([]byte, 100)), 100)
	if err == nil || !strings.Contains(err.Error(), "507") {
		t.Fatalf("err = %v, want the 507 surfaced", err)
	}
}

func TestCopyWithProgressPropagatesWriteErrors(t *testing.T) {
	c := testClient("http://unused")
	_, err := c.copyWithProgress(context.Background(), failTarget{}, strings.NewReader("hello"))
	if err == nil || !strings.Contains(err.Error(), "write at offset") {
		t.Fatalf("err = %v, want a write error naming the offset", err)
	}
}

type failTarget struct{}

func (failTarget) Write([]byte) (int, error) { return 0, fmt.Errorf("card is dead") }
func (failTarget) Sync() error               { return nil }

func TestDryrunReportFormatting(t *testing.T) {
	ok := DryrunReport{
		URL:     "http://h/golden.img.zst",
		MAC:     "b8:27:eb:01:02:03",
		Attempt: 2,
		Stats:   Stats{Bytes: 2 << 30, Duration: 100 * time.Second},
	}
	if got, want := ok.Result(), "OK attempt=2 2147483648 bytes in 1m40s (21.5 MB/s)"; got != want {
		t.Errorf("Result() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(ok.String(), "url: http://h/golden.img.zst\nmac: b8:27:eb:01:02:03\nresult: OK") {
		t.Errorf("String() = %q", ok.String())
	}

	failed := DryrunReport{URL: "u", Attempt: 3, Err: fmt.Errorf("boom")}
	if got, want := failed.Result(), "FAILED attempt=3: boom"; got != want {
		t.Errorf("Result() = %q, want %q", got, want)
	}
	if !strings.Contains(failed.String(), "mac: unknown") {
		t.Errorf("String() = %q, want an unknown MAC", failed.String())
	}
}

func TestStatsRate(t *testing.T) {
	if got := (Stats{Bytes: 100}).Rate(); got != 0 {
		t.Errorf("Rate with no duration = %v, want 0", got)
	}
	if got := (Stats{Bytes: 1000, Duration: 2 * time.Second}).Rate(); got != 500 {
		t.Errorf("Rate = %v, want 500", got)
	}
}

// TestUploadEncoderIsParallel pins the capture encoder's concurrency options.
// It reads unexported fields of the v1.19.2 zstd.Encoder; if the pinned
// library version changes, update the field names here.
func TestUploadEncoderIsParallel(t *testing.T) {
	enc, err := newUploadEncoder(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	o := reflect.ValueOf(enc).Elem().FieldByName("o")
	if !o.IsValid() {
		t.Fatal("zstd.Encoder no longer has field o; library layout changed")
	}
	if !o.FieldByName("concurrentBlocks").Bool() {
		t.Error("capture encoder does not have concurrent blocks enabled; capture will encode on one core at ~13 MB/s")
	}
	if got := o.FieldByName("concurrent").Int(); got != 3 {
		t.Errorf("capture encoder concurrency = %d, want exactly 3 (RAM budget on a 1 GB Pi)", got)
	}
}

// patternReader yields 4 KiB runs of a slowly-varying byte: compressible
// enough that a 100 MiB encode is fast, non-constant enough to be honest.
type patternReader struct{ off int64 }

func (p *patternReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte((p.off + int64(i)) >> 12)
	}
	p.off += int64(len(b))
	return len(b), nil
}

func TestUploadMultiJobRoundTrip(t *testing.T) {
	const size = 100 << 20 // > 3x the 32 MiB parallel job size

	want := sha256.New()
	if _, err := io.CopyN(want, &patternReader{}, size); err != nil {
		t.Fatal(err)
	}

	got := sha256.New()
	var received int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dec, err := zstd.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer dec.Close()
		n, err := io.Copy(got, dec.IOReadCloser())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		received = n
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stats, err := testClient(srv.URL+"/capture").Upload(context.Background(), &patternReader{}, size)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if stats.Bytes != size {
		t.Errorf("stats.Bytes = %d, want %d", stats.Bytes, size)
	}
	if received != size {
		t.Errorf("server decoded %d bytes, want %d", received, size)
	}
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Error("multi-job capture stream decoded to different bytes than the source")
	}
}

// sizedReader returns n bytes of unspecified content as fast as possible,
// so multi-GiB write patterns can be exercised without allocating them.
type sizedReader struct{ n int64 }

func (r *sizedReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	r.n -= int64(len(p))
	return len(p), nil
}

// rangeRecorder is a Target+RangeSyncer that discards writes and records
// the writeback call pattern.
type rangeRecorder struct {
	syncs    int
	calls    []string
	awaitErr error
}

func (r *rangeRecorder) Write(p []byte) (int, error) { return len(p), nil }
func (r *rangeRecorder) Sync() error                 { r.syncs++; return nil }
func (r *rangeRecorder) StartWriteback(off, n int64) error {
	r.calls = append(r.calls, fmt.Sprintf("start %d+%d", off, n))
	return nil
}
func (r *rangeRecorder) AwaitWriteback(off, n int64) error {
	r.calls = append(r.calls, fmt.Sprintf("await %d+%d", off, n))
	return r.awaitErr
}

// plainRecorder is a Target with no RangeSyncer, for the fallback path.
type plainRecorder struct{ syncs int }

func (p *plainRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (p *plainRecorder) Sync() error                 { p.syncs++; return nil }

func TestCopyWithProgressPipelinesWriteback(t *testing.T) {
	c := testClient("http://unused")
	rec := &rangeRecorder{}
	const interval = int64(ProgressInterval)
	n, err := c.copyWithProgress(context.Background(), rec, &sizedReader{n: 3*interval + interval/2})
	if err != nil {
		t.Fatalf("copyWithProgress: %v", err)
	}
	if want := 3*interval + interval/2; n != want {
		t.Fatalf("wrote %d bytes, want %d", n, want)
	}
	want := []string{
		fmt.Sprintf("start %d+%d", 0*interval, interval),
		fmt.Sprintf("start %d+%d", 1*interval, interval),
		fmt.Sprintf("await %d+%d", 0*interval, interval),
		fmt.Sprintf("start %d+%d", 2*interval, interval),
		fmt.Sprintf("await %d+%d", 1*interval, interval),
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Errorf("writeback calls = %v, want %v", rec.calls, want)
	}
	if rec.syncs != 0 {
		t.Errorf("copyWithProgress called Sync %d times on a RangeSyncer target; the blocking stall is back", rec.syncs)
	}
}

func TestStreamStillSyncsRangeSyncerTargets(t *testing.T) {
	image := zstdOf(t, bytes.Repeat([]byte("z"), 100000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(image)
	}))
	defer srv.Close()

	rec := &rangeRecorder{}
	if _, err := testClient(srv.URL).Stream(context.Background(), rec); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if rec.syncs != 1 {
		t.Errorf("Stream called Sync %d times, want exactly the final durability barrier", rec.syncs)
	}
}

func TestCopyWithProgressFallbackStillSyncs(t *testing.T) {
	c := testClient("http://unused")
	rec := &plainRecorder{}
	const interval = int64(ProgressInterval)
	if _, err := c.copyWithProgress(context.Background(), rec, &sizedReader{n: 2*interval + interval/2}); err != nil {
		t.Fatalf("copyWithProgress: %v", err)
	}
	if rec.syncs != 2 {
		t.Errorf("periodic syncs = %d, want 2 (dirty pages would grow unbounded on a 1 GB Pi)", rec.syncs)
	}
}

func TestCopyWithProgressAwaitErrorFailsAttempt(t *testing.T) {
	c := testClient("http://unused")
	rec := &rangeRecorder{awaitErr: fmt.Errorf("card fell out")}
	const interval = int64(ProgressInterval)
	_, err := c.copyWithProgress(context.Background(), rec, &sizedReader{n: 3 * interval})
	if err == nil || !strings.Contains(err.Error(), "awaiting writeback at offset 0") {
		t.Fatalf("err = %v, want an awaiting-writeback error naming offset 0", err)
	}
}
