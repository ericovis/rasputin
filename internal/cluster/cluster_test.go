package cluster

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/ericovis/rasputin/internal/mbr"
	"github.com/ericovis/rasputin/internal/server"
)

func TestStatusTable(t *testing.T) {
	rows := []Status{
		{Name: "rasputin001", MAC: "b8:27:eb:01:02:03", IP: "192.168.0.74", Reachable: true,
			SSHUser: "berry", Hostname: "rasputin001", BuildID: "20260828T1-abc",
			Provisioned: true, Uptime: "up 2 hours"},
		{Name: "rasputin003", MAC: "b8:27:eb:07:08:09", Reachable: false,
			Err: fmt.Errorf("no route to host")},
	}
	out := StatusTable(rows)
	for _, want := range []string{
		"NODE", "rasputin001", "192.168.0.74", "berry", "20260828T1-abc", "yes", "up 2 hours",
		"rasputin003", "down",
		"rasputin003: no route to host",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q:\n%s", want, out)
		}
	}
	// An unreachable node must not claim to be unprovisioned; it is unknown.
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "rasputin003 ") && strings.Contains(l, "no  ") {
			t.Errorf("an unreachable node reported a provisioning state: %q", l)
		}
	}
}

func TestStatusTableMarksAnAdoptedStockNode(t *testing.T) {
	out := StatusTable([]Status{{Name: "n", Reachable: true, SSHUser: "berry", Adopted: true}})
	if !strings.Contains(out, "stock (adopted)") {
		t.Errorf("table = %s", out)
	}
	plain := StatusTable([]Status{{Name: "n", Reachable: true, SSHUser: "berry"}})
	if !strings.Contains(plain, "stock") || strings.Contains(plain, "adopted") {
		t.Errorf("table = %s", plain)
	}
}

func TestBuildIDFrom(t *testing.T) {
	release := "build_id=20260828T233619Z-40fd3e\nbase=2026-06-18-raspios.img\nbaked_at=2026-08-28\n"
	if got := buildIDFrom(release); got != "20260828T233619Z-40fd3e" {
		t.Errorf("buildIDFrom = %q", got)
	}
	if got := buildIDFrom("nothing here\n"); got != "" {
		t.Errorf("buildIDFrom = %q, want empty", got)
	}
}

func TestGoldenMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	if _, err := ReadGoldenMeta(); err == nil {
		t.Error("ReadGoldenMeta accepted a missing file")
	}
	want := &GoldenMeta{
		BuildID: "20260828T1-abc", SHA256: "deadbeef", Bytes: 12345,
		Base: "base.img", Builder: "rasputin001", BakedAt: time.Now().UTC().Truncate(time.Second),
		CardUsed: 999,
	}
	if err := WriteGoldenMeta(want); err != nil {
		t.Fatalf("WriteGoldenMeta: %v", err)
	}
	got, err := ReadGoldenMeta()
	if err != nil {
		t.Fatalf("ReadGoldenMeta: %v", err)
	}
	if got.BuildID != want.BuildID || got.Bytes != want.Bytes || got.CardUsed != want.CardUsed {
		t.Errorf("meta = %+v, want %+v", got, want)
	}

	if err := os.WriteFile(GoldenMetaPath, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGoldenMeta(); err == nil {
		t.Error("ReadGoldenMeta accepted invalid JSON")
	}
}

// fakeCard builds a zstd-compressed disk image whose MBR claims exactly the
// bytes it contains, the way a real capture does.
func fakeCard(t *testing.T, p2Sectors uint32) []byte {
	t.Helper()
	const p1Start, p1Len = 8192, 1024
	sector := make([]byte, mbr.SectorSize)
	putEntry(sector, 0, 0x0c, p1Start, p1Len)
	putEntry(sector, 1, 0x83, p1Start+p1Len, p2Sectors)
	sector[510], sector[511] = 0x55, 0xAA

	total := int(p1Start+p1Len+p2Sectors) * mbr.SectorSize
	card := make([]byte, total)
	copy(card, sector)

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(card); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func putEntry(s []byte, i int, ptype byte, start, count uint32) {
	e := s[446+i*16:]
	e[4] = ptype
	putLE32(e[8:12], start)
	putLE32(e[12:16], count)
}

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func TestVerifyImageAcceptsAGoodCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "golden.img.zst")
	if err := os.WriteFile(path, fakeCard(t, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	used, err := verifyImage(path)
	if err != nil {
		t.Fatalf("verifyImage: %v", err)
	}
	if want := int64(8192+1024+4096) * mbr.SectorSize; used != want {
		t.Errorf("used = %d, want %d", used, want)
	}
}

func TestVerifyImageRejectsCorruption(t *testing.T) {
	good := fakeCard(t, 4096)
	corrupt := append([]byte(nil), good...)
	corrupt[len(corrupt)-3] ^= 0xff // break the frame checksum

	dir := t.TempDir()
	cases := map[string][]byte{
		"corrupt":   corrupt,
		"truncated": good[:len(good)/2],
		"not zstd":  []byte("this is not an image at all"),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".zst")
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyImage(path); err == nil {
				t.Fatal("verifyImage accepted an unusable image")
			}
		})
	}
	if _, err := verifyImage(filepath.Join(dir, "absent.zst")); err == nil {
		t.Error("verifyImage accepted a missing file")
	}
}

func TestVerifyImageRejectsASizeMismatch(t *testing.T) {
	// A card whose stream is longer than its partition table claims: either
	// the capture over-read or the table is wrong. Either way, not golden.
	const p2 = 4096
	card := fakeCard(t, p2)
	dec, err := zstd.NewReader(bytes.NewReader(card))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readAllAndClose(dec)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, make([]byte, mbr.SectorSize)...) // one sector too many

	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf, zstd.WithEncoderCRC(true))
	enc.Write(raw)
	enc.Close()

	path := filepath.Join(t.TempDir(), "long.zst")
	os.WriteFile(path, buf.Bytes(), 0o644)
	if _, err := verifyImage(path); err == nil {
		t.Fatal("verifyImage accepted an image longer than its partition table")
	}
}

func TestBestImagePrefersGolden(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	if _, err := BestImage(); err == nil {
		t.Error("BestImage found something in an empty directory")
	}

	os.MkdirAll("out", 0o755)
	os.WriteFile(VanillaImage.Path, []byte("vanilla"), 0o644)
	img, err := BestImage()
	if err != nil {
		t.Fatal(err)
	}
	if img.Name != VanillaImage.Name {
		t.Errorf("BestImage = %s, want the prepared image", img.Name)
	}

	os.WriteFile(GoldenImage.Path, []byte("golden"), 0o644)
	if img, _ = BestImage(); img.Name != GoldenImage.Name {
		t.Errorf("BestImage = %s, want the golden image", img.Name)
	}

	// An empty file is not an image.
	os.WriteFile(GoldenImage.Path, nil, 0o644)
	if img, _ = BestImage(); img.Name != VanillaImage.Name {
		t.Errorf("BestImage = %s, want the empty golden image ignored", img.Name)
	}
}

func TestProgressLine(t *testing.T) {
	p := server.Progress{
		Bytes: 100 << 20, Total: 700 << 20,
		Started: time.Now().Add(-10 * time.Second), Updated: time.Now(),
	}
	got := ProgressLine(p)
	for _, want := range []string{"100/700 MiB", "14%", "MB/s"} {
		if !strings.Contains(got, want) {
			t.Errorf("ProgressLine = %q, want it to contain %q", got, want)
		}
	}
	if got := ProgressLine(server.Progress{Bytes: 5 << 20}); !strings.Contains(got, "5 MiB") {
		t.Errorf("ProgressLine with no total = %q", got)
	}
}

func TestIsDisconnect(t *testing.T) {
	for _, s := range []string{"EOF", "connection reset by peer", "use of closed network connection", "broken pipe"} {
		if !isDisconnect(fmt.Errorf("%s", s)) {
			t.Errorf("%q should read as a disconnect", s)
		}
	}
	if isDisconnect(fmt.Errorf("permission denied")) {
		t.Error("permission denied should not read as a disconnect")
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { os.Chdir(old) }
}

func readAllAndClose(dec *zstd.Decoder) ([]byte, error) {
	defer dec.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(dec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
