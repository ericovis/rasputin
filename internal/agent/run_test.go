package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// fakeDisk stands in for /dev/mmcblk0.
type fakeDisk struct {
	written   bufTarget
	content   []byte
	used      int64
	openWrite int
	openErr   error
	closes    int
}

func (d *fakeDisk) OpenWrite() (Target, error) {
	d.openWrite++
	if d.openErr != nil {
		return nil, d.openErr
	}
	return &d.written, nil
}

func (d *fakeDisk) OpenRead() (io.ReadCloser, int64, error) {
	if d.openErr != nil {
		return nil, 0, d.openErr
	}
	return io.NopCloser(bytes.NewReader(d.content)), d.used, nil
}

func (d *fakeDisk) Close() error { d.closes++; return nil }

// fakeSystem records reboots and gives the runners real directories to treat
// as the boot partition and the writable layer.
type fakeSystem struct {
	dir      string
	upperDir string
	reboots  int
	bootErr  error
	upperErr error
}

func (s *fakeSystem) WithBoot(fn func(string) error) error {
	if s.bootErr != nil {
		return s.bootErr
	}
	return fn(s.dir)
}

func (s *fakeSystem) WithUpper(fn func(string) error) error {
	if s.upperErr != nil {
		return s.upperErr
	}
	return fn(s.upperDir)
}

func (s *fakeSystem) Reboot() error { s.reboots++; return nil }

// withUpperPart points HasUpper at a file a test controls, so a reset can be
// run on a build host that has no SD card. present=false models a node whose
// golden image predates the overlay.
func withUpperPart(t *testing.T, present bool) {
	t.Helper()
	oldPath, oldWait := upperPath, upperWait
	t.Cleanup(func() { upperPath, upperWait = oldPath, oldWait })
	upperWait = 0
	upperPath = filepath.Join(t.TempDir(), "mmcblk0p3")
	if present {
		if err := os.WriteFile(upperPath, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReflashWritesTheImageAndRetries(t *testing.T) {
	payload := bytes.Repeat([]byte("IMAGE"), 20000)
	image := zstdOf(t, payload)
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		// The first GET dies mid-stream, so the retry path is exercised.
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Write(image[:len(image)/2])
			return
		}
		w.Write(image)
	}))
	defer srv.Close()

	disk := &fakeDisk{}
	if err := Reflash(context.Background(), testClient(srv.URL), disk); err != nil {
		t.Fatalf("Reflash: %v", err)
	}
	if !bytes.HasSuffix(disk.written.Bytes(), payload) {
		t.Errorf("card holds %d bytes, want it to end with the %d-byte image", disk.written.Len(), len(payload))
	}
	if disk.openWrite != 2 {
		t.Errorf("opened the card %d times, want 2 (one failed attempt, one good)", disk.openWrite)
	}
	if disk.closes != disk.openWrite {
		t.Errorf("closed the card %d times for %d opens", disk.closes, disk.openWrite)
	}
}

func TestReflashNeverOpensTheCardBeforeAGoodProbe(t *testing.T) {
	// The server is never reachable and the context is cancelled from under
	// the probe loop: the card must not have been touched.
	ctx, cancel := context.WithCancel(context.Background())
	c := testClient("http://127.0.0.1:1/nothing")
	c.Sleep = func(context.Context, time.Duration) {}
	disk := &fakeDisk{}
	go cancel()
	if err := Reflash(ctx, c, disk); err == nil {
		t.Fatal("Reflash returned success with an unreachable server")
	}
	if disk.openWrite != 0 {
		t.Errorf("the card was opened %d times before any successful probe", disk.openWrite)
	}
}

func TestDryrunSucceedsAndNeverTouchesTheCard(t *testing.T) {
	payload := bytes.Repeat([]byte("D"), 50000)
	image := zstdOf(t, payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(image)
	}))
	defer srv.Close()

	sys := &fakeSystem{dir: t.TempDir()}
	flag := filepath.Join(sys.dir, FlagDryrun)
	if err := os.WriteFile(flag, []byte(srv.URL), 0o644); err != nil {
		t.Fatal(err)
	}
	disk := &fakeDisk{}

	if err := RunMode(context.Background(), ModeDryrun, testClient(srv.URL), disk, sys); err != nil {
		t.Fatalf("RunMode: %v", err)
	}
	if disk.openWrite != 0 {
		t.Error("a dryrun opened the card for writing")
	}
	if sys.reboots != 1 {
		t.Errorf("reboots = %d, want 1", sys.reboots)
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Error("the dryrun flag was not removed")
	}
	log, err := os.ReadFile(filepath.Join(sys.dir, DryrunLog))
	if err != nil {
		t.Fatalf("reading the report: %v", err)
	}
	if !strings.Contains(string(log), "result: OK attempt=1") {
		t.Errorf("report = %q", log)
	}
	if !strings.Contains(string(log), fmt.Sprintf("%d bytes", len(payload))) {
		t.Errorf("report does not mention the byte count: %q", log)
	}
}

func TestDryrunGivesUpAfterThreeAttempts(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		atomic.AddInt32(&gets, 1)
		w.Write([]byte("this is not zstd"))
	}))
	defer srv.Close()

	report := Dryrun(context.Background(), testClient(srv.URL))
	if report.Err == nil {
		t.Fatal("Dryrun reported success on a garbage stream")
	}
	if report.Attempt != DryrunAttempts {
		t.Errorf("attempt = %d, want %d", report.Attempt, DryrunAttempts)
	}
	if got := atomic.LoadInt32(&gets); got != int32(DryrunAttempts) {
		t.Errorf("server saw %d GETs, want %d", got, DryrunAttempts)
	}
	if !strings.HasPrefix(report.Result(), "FAILED attempt=3") {
		t.Errorf("Result() = %q", report.Result())
	}
}

func TestDryrunReportsAnUnreachableServer(t *testing.T) {
	c := testClient("http://127.0.0.1:1/nothing")
	report := Dryrun(context.Background(), c)
	if report.Err == nil {
		t.Fatal("want a failure against an unreachable server")
	}
	if !strings.Contains(report.Result(), "not reachable") {
		t.Errorf("Result() = %q", report.Result())
	}
}

func TestCaptureUploadsUsedBytesAndClearsFlag(t *testing.T) {
	card := bytes.Repeat([]byte("CARD"), 30000)
	const used = 60000
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dec, err := zstd.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		received, _ = io.ReadAll(dec)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sys := &fakeSystem{dir: t.TempDir()}
	flag := filepath.Join(sys.dir, FlagCapture)
	if err := os.WriteFile(flag, []byte(srv.URL), 0o644); err != nil {
		t.Fatal(err)
	}
	disk := &fakeDisk{content: card, used: used}

	if err := RunMode(context.Background(), ModeCapture, testClient(srv.URL), disk, sys); err != nil {
		t.Fatalf("RunMode: %v", err)
	}
	if !bytes.Equal(received, card[:used]) {
		t.Errorf("server received %d bytes, want the first %d", len(received), used)
	}
	if disk.openWrite != 0 {
		t.Error("a capture opened the card for writing")
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Error("the capture flag was not removed")
	}
	if sys.reboots != 1 {
		t.Errorf("reboots = %d, want 1", sys.reboots)
	}
}

// TestCaptureClearsItsFlagBeforeReadingTheCard is the regression test for a
// self-replicating bug found on hardware: the capture flag was removed only
// after the card had been streamed, so the flag was still on the boot
// partition while it was being read — and ended up inside the golden image.
// Every node flashed from that image then booted, found a capture flag, and
// tried to upload its own card.
func TestCaptureClearsItsFlagBeforeReadingTheCard(t *testing.T) {
	var flagPresentDuringRead bool
	sys := &fakeSystem{dir: t.TempDir()}
	flag := filepath.Join(sys.dir, FlagCapture)
	if err := os.WriteFile(flag, []byte("http://server/capture"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	disk := &observingDisk{
		content: bytes.Repeat([]byte("CARD"), 1000),
		used:    4000,
		onRead: func() {
			_, err := os.Stat(flag)
			flagPresentDuringRead = err == nil
		},
	}

	if err := RunMode(context.Background(), ModeCapture, testClient(srv.URL), disk, sys); err != nil {
		t.Fatalf("RunMode: %v", err)
	}
	if flagPresentDuringRead {
		t.Error("the capture flag was still on the boot partition while the card was being read; " +
			"it would be baked into the captured image")
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Error("the capture flag survived")
	}
}

// TestCaptureRefusesWhenTheFlagCannotBeCleared proves the agent would rather
// produce no image than a poisoned one.
func TestCaptureRefusesWhenTheFlagCannotBeCleared(t *testing.T) {
	sys := &fakeSystem{dir: t.TempDir(), bootErr: fmt.Errorf("boot partition is read-only")}
	disk := &observingDisk{content: []byte("card"), used: 4}
	err := RunMode(context.Background(), ModeCapture, testClient("http://unused"), disk, sys)
	if err == nil {
		t.Fatal("the agent captured without clearing its flag")
	}
	if !strings.Contains(err.Error(), "poison") {
		t.Errorf("err = %v, want it to explain the risk", err)
	}
	if disk.reads != 0 {
		t.Error("the card was read even though the flag could not be cleared")
	}
	if sys.reboots != 0 {
		t.Error("the node rebooted despite refusing to capture")
	}
}

// observingDisk reports when its card is read, so a test can inspect the
// boot partition at that exact moment.
type observingDisk struct {
	content []byte
	used    int64
	reads   int
	onRead  func()
}

func (d *observingDisk) OpenWrite() (Target, error) {
	return nil, fmt.Errorf("a capture must never open the card for writing")
}

func (d *observingDisk) OpenRead() (io.ReadCloser, int64, error) {
	d.reads++
	if d.onRead != nil {
		d.onRead()
	}
	return io.NopCloser(bytes.NewReader(d.content)), d.used, nil
}

func (d *observingDisk) Close() error { return nil }

func TestRunModeRejectsNormal(t *testing.T) {
	err := RunMode(context.Background(), ModeNormal, testClient("http://x"), &fakeDisk{}, &fakeSystem{dir: t.TempDir()})
	if err == nil {
		t.Fatal("RunMode accepted a non-network mode")
	}
}

func TestRunModeRebootsEvenIfTheReportCannotBeWritten(t *testing.T) {
	image := zstdOf(t, []byte("small"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(image)
	}))
	defer srv.Close()
	sys := &fakeSystem{dir: t.TempDir(), bootErr: fmt.Errorf("boot partition is read-only")}
	if err := RunMode(context.Background(), ModeDryrun, testClient(srv.URL), &fakeDisk{}, sys); err != nil {
		t.Fatalf("RunMode: %v", err)
	}
	if sys.reboots != 1 {
		t.Errorf("reboots = %d, want 1 despite the report failing", sys.reboots)
	}
}

func TestRemoveFlagIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := removeFlag(dir, FlagReflash); err != nil {
		t.Errorf("removing an absent flag: %v", err)
	}
}

// resetSystem is a fakeSystem with both a boot partition and a writable layer
// that already holds something to throw away.
func resetSystem(t *testing.T) *fakeSystem {
	t.Helper()
	sys := &fakeSystem{dir: t.TempDir(), upperDir: t.TempDir()}
	for _, sub := range []string{UpperSubdir, WorkSubdir} {
		path := filepath.Join(sys.upperDir, sub, "var", "log")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "caramelo.log"), []byte("noise"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sys.dir, FlagReset), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return sys
}

// TestResetEmptiesTheLayerAndContinuesTheBoot is what makes a reset worth
// having: no network, no card write, no reboot — the same boot carries on
// into a system that is the golden image again.
func TestResetEmptiesTheLayerAndContinuesTheBoot(t *testing.T) {
	withUpperPart(t, true)
	sys := resetSystem(t)
	disk := &fakeDisk{}

	if err := RunMode(context.Background(), ModeReset, testClient("http://unused"), disk, sys); err != nil {
		t.Fatalf("RunMode: %v", err)
	}
	for _, sub := range []string{UpperSubdir, WorkSubdir} {
		entries, err := os.ReadDir(filepath.Join(sys.upperDir, sub))
		if err != nil {
			t.Fatalf("reading %s back: %v", sub, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s still holds %d entries", sub, len(entries))
		}
	}
	if _, err := os.Stat(filepath.Join(sys.dir, FlagReset)); !os.IsNotExist(err) {
		t.Error("the reset flag was not removed; the node would reset again on every boot")
	}
	if sys.reboots != 0 {
		t.Errorf("reboots = %d, want 0: a reset boots straight on", sys.reboots)
	}
	if disk.openWrite != 0 || disk.closes != 0 {
		t.Error("a reset touched the card")
	}
}

// TestResetWithoutAWritableLayerIsHarmless: a node still running a golden
// image from before the overlay has nothing to reset, and must not be left
// with a flag file that makes every boot try again.
func TestResetWithoutAWritableLayerIsHarmless(t *testing.T) {
	withUpperPart(t, false)
	sys := resetSystem(t)

	if err := RunMode(context.Background(), ModeReset, testClient("http://unused"), nil, sys); err != nil {
		t.Fatalf("RunMode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sys.dir, FlagReset)); !os.IsNotExist(err) {
		t.Error("the reset flag survived on a node with no writable layer")
	}
	// The layer the fake holds is not the node's, and must not be touched.
	if _, err := os.Stat(filepath.Join(sys.upperDir, UpperSubdir, "var", "log", "caramelo.log")); err != nil {
		t.Errorf("a node with no writable layer had one wiped anyway: %v", err)
	}
}

// TestResetKeepsItsFlagWhenTheWipeFails: the flag is the instruction, so it
// only goes once the work it asks for is done. Otherwise a failed reset would
// look like a finished one.
func TestResetKeepsItsFlagWhenTheWipeFails(t *testing.T) {
	withUpperPart(t, true)
	sys := resetSystem(t)
	sys.upperErr = fmt.Errorf("mount: no such device")

	err := RunMode(context.Background(), ModeReset, testClient("http://unused"), nil, sys)
	if err == nil {
		t.Fatal("RunMode reported a successful reset although the layer could not be mounted")
	}
	if !strings.Contains(err.Error(), "writable layer") {
		t.Errorf("err = %v, want it to name the writable layer", err)
	}
	if _, err := os.Stat(filepath.Join(sys.dir, FlagReset)); err != nil {
		t.Error("the reset flag was cleared although the wipe failed; the reset would never happen")
	}
}
