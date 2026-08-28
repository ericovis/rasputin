package vanilla

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

func TestUnitImageName(t *testing.T) {
	cases := map[string]string{
		"https://downloads.raspberrypi.com/raspios_lite_arm64/images/2026-08-01-raspios-trixie-arm64-lite.img.xz": "2026-08-01-raspios-trixie-arm64-lite.img",
		"https://h/x/thing.img.xz":         "thing.img",
		"https://h/x/thing.img":            "thing.img",
		"https://h/x/thing":                "thing.img",
		"https://h/x/thing.img.xz?token=1": "thing.img",
		"https://h/":                       "raspios.img",
	}
	for in, want := range cases {
		if got := ImageName(in); got != want {
			t.Errorf("ImageName(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeXZ compresses payload the way the Raspberry Pi download does.
func fakeXZ(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// imageServer serves compressed at /images/<name>, behind a redirect from
// /latest, mirroring the real download endpoint.
func imageServer(t *testing.T, name string, compressed []byte, hits *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/images/"+name, http.StatusFound)
	})
	mux.HandleFunc("/images/"+name, func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "application/x-xz")
		w.Write(compressed)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUnitEnsureDownloadsDecodesAndRecordsMeta(t *testing.T) {
	payload := bytes.Repeat([]byte("PI-IMAGE"), 5000)
	compressed := fakeXZ(t, payload)
	var hits int
	srv := imageServer(t, "2026-08-01-raspios-trixie-arm64-lite.img.xz", compressed, &hits)

	dir := t.TempDir()
	opts := Options{
		SourceURL: srv.URL + "/latest",
		CacheDir:  filepath.Join(dir, "cache"),
		MetaPath:  filepath.Join(dir, "out/meta/vanilla.json"),
	}
	imgPath, meta, err := Ensure(opts)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if filepath.Base(imgPath) != "2026-08-01-raspios-trixie-arm64-lite.img" {
		t.Errorf("image path = %q", imgPath)
	}
	got, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("reading the decoded image: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decoded %d bytes, want %d", len(got), len(payload))
	}
	if meta.Bytes != int64(len(payload)) || meta.CompressedBytes != int64(len(compressed)) {
		t.Errorf("meta sizes = %d/%d, want %d/%d", meta.Bytes, meta.CompressedBytes, len(payload), len(compressed))
	}
	if want := sha256hex(payload); meta.SHA256Img != want {
		t.Errorf("sha256_img = %s, want %s", meta.SHA256Img, want)
	}
	if want := sha256hex(compressed); meta.SHA256XZ != want {
		t.Errorf("sha256_xz = %s, want %s", meta.SHA256XZ, want)
	}
	if !strings.HasSuffix(meta.ResolvedURL, "/images/2026-08-01-raspios-trixie-arm64-lite.img.xz") {
		t.Errorf("resolved_url = %q, want the post-redirect URL", meta.ResolvedURL)
	}
	if meta.FetchedAt.IsZero() {
		t.Error("fetched_at not recorded")
	}

	onDisk, err := ReadMeta(opts.MetaPath)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if onDisk.SHA256Img != meta.SHA256Img {
		t.Error("vanilla.json does not match the returned meta")
	}

	// Second call must be served from the cache.
	if _, _, err := Ensure(opts); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if hits != 1 {
		t.Errorf("server was hit %d times, want 1 (the second call should use the cache)", hits)
	}
}

func TestUnitEnsureRefetchesWhenTheCacheIsTruncated(t *testing.T) {
	payload := bytes.Repeat([]byte("Z"), 10000)
	var hits int
	srv := imageServer(t, "img.img.xz", fakeXZ(t, payload), &hits)

	dir := t.TempDir()
	opts := Options{
		SourceURL: srv.URL + "/latest",
		CacheDir:  filepath.Join(dir, "cache"),
		MetaPath:  filepath.Join(dir, "vanilla.json"),
	}
	imgPath, _, err := Ensure(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imgPath, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Ensure(opts); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Errorf("server was hit %d times, want 2 (a truncated cache must be refetched)", hits)
	}
}

func TestUnitEnsureRefetchesWhenTheImageIsMissing(t *testing.T) {
	payload := bytes.Repeat([]byte("Q"), 5000)
	var hits int
	srv := imageServer(t, "img.img.xz", fakeXZ(t, payload), &hits)
	dir := t.TempDir()
	opts := Options{SourceURL: srv.URL + "/latest", CacheDir: filepath.Join(dir, "c"), MetaPath: filepath.Join(dir, "m.json")}
	imgPath, _, err := Ensure(opts)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(imgPath)
	if _, _, err := Ensure(opts); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Errorf("server was hit %d times, want 2", hits)
	}
}

func TestUnitEnsureLeavesNoPartialImageOnFailure(t *testing.T) {
	// A stream that is valid xz for a while and then stops mid-frame.
	full := fakeXZ(t, bytes.Repeat([]byte("W"), 200000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(full[:len(full)/2])
	}))
	defer srv.Close()

	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	_, _, err := Ensure(Options{SourceURL: srv.URL + "/x.img.xz", CacheDir: cacheDir, MetaPath: filepath.Join(dir, "m.json")})
	if err == nil {
		t.Fatal("a truncated download was accepted")
	}
	entries, rerr := os.ReadDir(cacheDir)
	if rerr != nil {
		t.Fatalf("reading the cache dir: %v", rerr)
	}
	for _, e := range entries {
		t.Errorf("cache holds %q after a failed download, want nothing", e.Name())
	}
	if _, err := os.Stat(filepath.Join(dir, "m.json")); err == nil {
		t.Error("vanilla.json was written for a failed download")
	}
}

func TestUnitEnsureRejectsBadResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if _, _, err := Ensure(Options{SourceURL: srv.URL, CacheDir: dir, MetaPath: filepath.Join(dir, "m.json")}); err == nil {
		t.Error("a 404 was accepted")
	}

	notXZ := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not xz at all"))
	}))
	defer notXZ.Close()
	if _, _, err := Ensure(Options{SourceURL: notXZ.URL, CacheDir: dir, MetaPath: filepath.Join(dir, "m2.json")}); err == nil {
		t.Error("a non-xz body was accepted")
	}

	if _, _, err := Ensure(Options{}); err == nil {
		t.Error("an empty SourceURL was accepted")
	}
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
