package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testServer starts a server bound to loopback, skipping LAN-IP detection.
func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := Start(Options{
		BindIP: net.ParseIP("127.0.0.1"),
		OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func writeImage(t *testing.T, dir, name string, size int) (string, []byte) {
	t.Helper()
	content := bytes.Repeat([]byte("IMAGE-DATA-"), size/11+1)[:size]
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, content
}

func TestServeImageAndTrackProgress(t *testing.T) {
	s := testServer(t)
	dir := t.TempDir()
	path, content := writeImage(t, dir, "golden.img.zst", 300000)
	if err := s.Register("golden.img.zst", path); err != nil {
		t.Fatalf("Register: %v", err)
	}

	url := s.URLFor("golden.img.zst")
	if !strings.HasPrefix(url, s.BaseURL()+ImagePrefix) {
		t.Errorf("URLFor = %q", url)
	}

	req, _ := http.NewRequest(http.MethodGet, url+"?mac=b8:27:eb:01:02:03", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("served %d bytes, want %d", len(got), len(content))
	}

	p, ok := s.ProgressFor("b8:27:eb:01:02:03")
	if !ok {
		t.Fatalf("no progress recorded; have %+v", s.Progress())
	}
	if p.Bytes != int64(len(content)) || p.Total != int64(len(content)) {
		t.Errorf("progress = %d/%d, want %d/%d", p.Bytes, p.Total, len(content), len(content))
	}
	if !p.Done {
		t.Error("progress not marked done")
	}
	if p.Percent() != 100 {
		t.Errorf("Percent = %v, want 100", p.Percent())
	}
	if p.MAC != "b8:27:eb:01:02:03" || p.IP == "" {
		t.Errorf("progress client = %+v", p)
	}
	if p.Image != "golden.img.zst" {
		t.Errorf("progress image = %q", p.Image)
	}

	s.ResetProgress()
	if len(s.Progress()) != 0 {
		t.Error("ResetProgress did not clear the table")
	}
}

func TestProgressUsesTheMACHeaderWhenTheQueryIsAbsent(t *testing.T) {
	s := testServer(t)
	dir := t.TempDir()
	path, _ := writeImage(t, dir, "img.zst", 1000)
	s.Register("img.zst", path)

	req, _ := http.NewRequest(http.MethodGet, s.URLFor("img.zst"), nil)
	req.Header.Set(MACHeader, "B8:27:EB:04:05:06")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if _, ok := s.ProgressFor("b8:27:eb:04:05:06"); !ok {
		t.Errorf("MAC header was not used for attribution; progress = %+v", s.Progress())
	}
}

func TestProgressFallsBackToTheClientIP(t *testing.T) {
	s := testServer(t)
	dir := t.TempDir()
	path, _ := writeImage(t, dir, "img.zst", 500)
	s.Register("img.zst", path)

	resp, err := http.Get(s.URLFor("img.zst"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	all := s.Progress()
	if len(all) != 1 {
		t.Fatalf("progress = %+v, want one entry", all)
	}
	if all[0].MAC != "" || all[0].Client != "127.0.0.1" {
		t.Errorf("progress = %+v, want it keyed by IP", all[0])
	}
}

func TestHeadProbeDoesNotStartATransfer(t *testing.T) {
	s := testServer(t)
	dir := t.TempDir()
	path, content := writeImage(t, dir, "img.zst", 4096)
	s.Register("img.zst", path)

	resp, err := http.Head(s.URLFor("img.zst") + "?mac=aa:bb")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD status = %s", resp.Status)
	}
	if got := resp.Header.Get("Content-Length"); got != fmt.Sprint(len(content)) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, len(content))
	}
	if len(s.Progress()) != 0 {
		t.Errorf("a probe created progress entries: %+v", s.Progress())
	}
}

func TestUnknownImageIs404(t *testing.T) {
	s := testServer(t)
	resp, err := http.Get(s.URLFor("nope.zst"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %s, want 404", resp.Status)
	}
}

func TestRegisterRejectsAMissingFile(t *testing.T) {
	s := testServer(t)
	if err := s.Register("x", filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("Register accepted a missing file")
	}
}

func TestCaptureStreamsToTheRegisteredDestination(t *testing.T) {
	s := testServer(t)
	dest := filepath.Join(t.TempDir(), "golden.img.zst")
	done, err := s.ExpectCapture("build-1", dest)
	if err != nil {
		t.Fatalf("ExpectCapture: %v", err)
	}

	payload := bytes.Repeat([]byte("CARD"), 200000)
	resp, err := http.Post(s.CaptureURL("build-1")+"&mac=b8:27:eb:01:02:03", "application/zstd", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the done channel never closed")
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading the captured file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("captured %d bytes, want %d", len(got), len(payload))
	}
	res, ok := s.CaptureResult("build-1")
	if !ok {
		t.Fatal("no CaptureResult")
	}
	if res.Bytes != int64(len(payload)) {
		t.Errorf("result bytes = %d, want %d", res.Bytes, len(payload))
	}
	sum := sha256.Sum256(payload)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("result sha256 = %s", res.SHA256)
	}
	if res.MAC != "b8:27:eb:01:02:03" {
		t.Errorf("result MAC = %q", res.MAC)
	}
	if res.Err != nil {
		t.Errorf("result err = %v", res.Err)
	}
	// The staging file must be gone.
	if _, err := os.Stat(s.StagePath("build-1")); !os.IsNotExist(err) {
		t.Error("the staging file survived a successful capture")
	}
}

func TestCaptureRejectsAnUnknownID(t *testing.T) {
	s := testServer(t)
	resp, err := http.Post(s.CaptureURL("never-registered"), "application/zstd", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %s, want 404 for an unregistered id", resp.Status)
	}
}

func TestCaptureRejectsGET(t *testing.T) {
	s := testServer(t)
	s.ExpectCapture("b", filepath.Join(t.TempDir(), "x"))
	resp, err := http.Get(s.CaptureURL("b"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %s, want 405", resp.Status)
	}
}

func TestCaptureIsSingleFlight(t *testing.T) {
	s := testServer(t)
	dest := filepath.Join(t.TempDir(), "golden.zst")
	if _, err := s.ExpectCapture("dup", dest); err != nil {
		t.Fatal(err)
	}

	// The first POST blocks until released, so the second arrives while it
	// is still streaming.
	release := make(chan struct{})
	slow := &blockingReader{data: bytes.Repeat([]byte("x"), 1000), release: release}

	var wg sync.WaitGroup
	var firstStatus int
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(s.CaptureURL("dup"), "application/zstd", slow)
		if err != nil {
			return
		}
		firstStatus = resp.StatusCode
		resp.Body.Close()
	}()

	// Wait until the first upload is genuinely in flight.
	deadline := time.Now().Add(3 * time.Second)
	for !slow.started() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := http.Post(s.CaptureURL("dup"), "application/zstd", strings.NewReader("second"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("concurrent capture status = %s, want 409", resp.Status)
	}
	close(release)
	wg.Wait()
	if firstStatus != http.StatusOK {
		t.Errorf("the first capture finished with %d, want 200", firstStatus)
	}
}

func TestCaptureRepeatAfterSuccessIsAccepted(t *testing.T) {
	// A node that does not hear the 200 retries; saying yes again is kinder
	// than making it re-stream a card that already arrived.
	s := testServer(t)
	dest := filepath.Join(t.TempDir(), "golden.zst")
	s.ExpectCapture("again", dest)
	for i := 0; i < 2; i++ {
		resp, err := http.Post(s.CaptureURL("again"), "application/zstd", strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d status = %s", i+1, resp.Status)
		}
	}
}

func TestExpectCaptureValidatesItsArguments(t *testing.T) {
	s := testServer(t)
	if _, err := s.ExpectCapture("", "dest"); err == nil {
		t.Error("an empty id was accepted")
	}
	if _, err := s.ExpectCapture("id", ""); err == nil {
		t.Error("an empty destination was accepted")
	}
	s.ExpectCapture("id", "dest")
	if _, err := s.ExpectCapture("id", "dest2"); err == nil {
		t.Error("a duplicate id was accepted")
	}
}

func TestLANIPIsNotLoopback(t *testing.T) {
	ip, err := LANIP()
	if err != nil {
		t.Skipf("no LAN route on this machine: %v", err)
	}
	if ip.IsLoopback() {
		t.Errorf("LANIP returned the loopback address %v", ip)
	}
}

func TestProgressRateAndPercentOnEmptyTransfers(t *testing.T) {
	var p Progress
	if p.Percent() != 0 || p.Rate() != 0 {
		t.Errorf("empty progress = %v%%, %v B/s", p.Percent(), p.Rate())
	}
}

// blockingReader releases its body only when told, so a test can hold an
// upload open.
type blockingReader struct {
	data    []byte
	off     int
	release chan struct{}
	mu      sync.Mutex
	begun   bool
}

func (b *blockingReader) started() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.begun
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.mu.Lock()
	first := !b.begun
	b.begun = true
	b.mu.Unlock()
	if first {
		// Hand over one byte so the handler is definitely inside the copy,
		// then wait.
		p[0] = b.data[0]
		b.off = 1
		return 1, nil
	}
	<-b.release
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}
