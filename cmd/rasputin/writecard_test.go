package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/mbr"
)

// cardBytes is what the test image decodes to: an MBR with a boot and a root
// partition, and a body of exactly the bytes that table accounts for, the way
// a capture is. Small enough to write in a test, the same shape as a golden.
func cardBytes() []byte {
	const p1Start, p1Len, p2Len = 8192, 1024, 4096
	card := make([]byte, int(p1Start+p1Len+p2Len)*mbr.SectorSize)
	putEntry(card, 0, 0x0c, p1Start, p1Len)
	putEntry(card, 1, 0x83, p1Start+p1Len, p2Len)
	card[510], card[511] = 0x55, 0xAA
	for i := mbr.SectorSize; i < len(card); i++ {
		card[i] = byte(i % 251)
	}
	return card
}

func putEntry(s []byte, i int, ptype byte, start, count uint32) {
	e := s[446+i*16:]
	e[4] = ptype
	e[8], e[9], e[10], e[11] = byte(start), byte(start>>8), byte(start>>16), byte(start>>24)
	e[12], e[13], e[14], e[15] = byte(count), byte(count>>8), byte(count>>16), byte(count>>24)
}

func zstdOf(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// builtImages points the command at a temporary out/ holding a golden image
// and its provenance — never the repository's — and returns the directory.
func builtImages(t *testing.T, card []byte) string {
	t.Helper()
	dir := t.TempDir()
	golden, vanilla, meta := goldenPath, vanillaPath, goldenMetaPath
	goldenPath = filepath.Join(dir, "golden.img.zst")
	vanillaPath = filepath.Join(dir, "vanilla-custom.img.zst")
	goldenMetaPath = filepath.Join(dir, "golden.json")
	t.Cleanup(func() { goldenPath, vanillaPath, goldenMetaPath = golden, vanilla, meta })

	if err := os.WriteFile(goldenPath, zstdOf(t, card), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cluster.GoldenMeta{BuildID: "20260908T231900Z-cf9898"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenMetaPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestWriteCardWritesTheImage is the command end to end with a file for a
// card: the bytes on the other side are the image, and the result says what
// went where.
func TestWriteCardWritesTheImage(t *testing.T) {
	card := cardBytes()
	dir := builtImages(t, card)
	dst := filepath.Join(dir, "card.img")

	var buf bytes.Buffer
	if err := runWriteCard(upConfig(t), jsonOutput(&buf), []string{"-device", dst, "-yes"}); err != nil {
		t.Fatalf("write-card: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, card) {
		t.Fatalf("the written card is %d bytes, want the %d bytes of the image", len(got), len(card))
	}

	res := result(t, &buf)
	if res["ok"] != true || res["device"] != dst || res["image"] != goldenPath {
		t.Errorf("result = %v", res)
	}
	if res["build_id"] != "20260908T231900Z-cf9898" {
		t.Errorf("build_id = %v, want the golden image's", res["build_id"])
	}
	if res["bytes"] != float64(len(card)) {
		t.Errorf("bytes = %v, want %d", res["bytes"], len(card))
	}
	if _, ok := res["duration_seconds"].(float64); !ok {
		t.Errorf("duration_seconds = %v, want a number", res["duration_seconds"])
	}
}

// TestWriteCardWithoutProvenance: a golden image whose out/meta is gone is
// still the bytes a node with a dead card needs. It is written, without a
// build id.
func TestWriteCardWithoutProvenance(t *testing.T) {
	dir := builtImages(t, cardBytes())
	if err := os.Remove(goldenMetaPath); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := runWriteCard(upConfig(t), jsonOutput(&buf), []string{"-device", filepath.Join(dir, "card.img"), "-yes"}); err != nil {
		t.Fatalf("write-card without out/meta: %v", err)
	}
	if res := result(t, &buf); res["build_id"] != nil {
		t.Errorf("build_id = %v, want it absent", res["build_id"])
	}
}

// TestWriteCardRejectsAnImageThatIsShortOfItsTable: the rule bake applies to
// a capture, applied here — a card written from a truncated image has half a
// root filesystem and boots into a repair shell.
func TestWriteCardRejectsAnImageThatIsShortOfItsTable(t *testing.T) {
	card := cardBytes()
	dir := builtImages(t, card[:len(card)-mbr.SectorSize])

	var buf bytes.Buffer
	err := runWriteCard(upConfig(t), jsonOutput(&buf), []string{"-device", filepath.Join(dir, "card.img"), "-yes"})
	if err == nil {
		t.Fatal("write-card accepted an image shorter than its partition table")
	}
	if !strings.Contains(err.Error(), "partition table claims") {
		t.Errorf("error = %v, want it to name the mismatch", err)
	}
}

// TestWriteCardInJSONModeWantsToBeTold: a program cannot pick a disk from a
// list or answer a prompt, so both refusals have to name the flag that means
// it.
func TestWriteCardInJSONModeWantsToBeTold(t *testing.T) {
	dir := builtImages(t, cardBytes())
	dst := filepath.Join(dir, "card.img")

	var buf bytes.Buffer
	err := runWriteCard(upConfig(t), jsonOutput(&buf), nil)
	if err == nil || !strings.Contains(err.Error(), "-device") {
		t.Errorf("write-card -json with no device = %v, want it to ask for -device", err)
	}

	buf.Reset()
	err = runWriteCard(upConfig(t), jsonOutput(&buf), []string{"-device", dst})
	if err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Errorf("write-card -json without -yes = %v, want it to ask for -yes", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("the refused write created the target anyway")
	}
	if buf.Len() > 0 {
		t.Errorf("the refusals wrote to stdout:\n%s", buf.String())
	}
}

func TestWriteCardRejectsAnUnknownImage(t *testing.T) {
	builtImages(t, cardBytes())
	var buf bytes.Buffer
	err := runWriteCard(upConfig(t), testOutput(&buf), []string{"-image", "stock", "-device", "/dev/null", "-yes"})
	if err == nil || !strings.Contains(err.Error(), "golden") {
		t.Fatalf("error = %v, want it to list the images there are", err)
	}
}

// TestWriteCardNeedsTheImageToBeThere: the order of artifacts is the same one
// flash enforces, and the error says which command builds the missing one.
func TestWriteCardNeedsTheImageToBeThere(t *testing.T) {
	dir := builtImages(t, cardBytes())
	if err := os.Remove(goldenPath); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "card.img")

	var buf bytes.Buffer
	err := runWriteCard(upConfig(t), testOutput(&buf), []string{"-device", dst, "-yes"})
	if err == nil || !strings.Contains(err.Error(), "rasputin bake") {
		t.Errorf("error = %v, want it to name the command that builds the image", err)
	}
	buf.Reset()
	err = runWriteCard(upConfig(t), testOutput(&buf), []string{"-image", "vanilla", "-device", dst, "-yes"})
	if err == nil || !strings.Contains(err.Error(), "rasputin prepare") {
		t.Errorf("error = %v, want it to name the command that prepares the image", err)
	}
}

// TestWriteCardPrintsProgressAndASummaryInTextMode: the operator watching a
// card write needs to see it moving, and to be told what was written.
func TestWriteCardPrintsProgressAndASummaryInTextMode(t *testing.T) {
	card := cardBytes()
	dir := builtImages(t, card)

	var buf bytes.Buffer
	if err := runWriteCard(upConfig(t), testOutput(&buf), []string{"-device", filepath.Join(dir, "card.img"), "-yes"}); err != nil {
		t.Fatalf("write-card: %v", err)
	}
	for _, want := range []string{"written ", "wrote ", "card.img"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, buf.String())
		}
	}
}

// TestWriteCardConfirmationSaysWhatIsErased: the one command whose mistake
// happens on the operator's own machine has to name the disk and the image
// before it asks.
func TestWriteCardConfirmationSaysWhatIsErased(t *testing.T) {
	var buf bytes.Buffer
	target := cardTarget{Path: "/dev/disk4", Detail: "31.9 GB SD Card Reader"}
	img := cardImage{Path: "out/golden.img.zst", BuildID: "build-1"}

	ok, err := confirmWriteCard(jsonOutput(&buf), nil, target, img)
	if ok || err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Fatalf("confirmWriteCard in JSON mode = (%v, %v), want a refusal naming -yes", ok, err)
	}
	ok, err = confirmWriteCard(testOutput(&buf), nil, target, img)
	if ok || err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Fatalf("confirmWriteCard without a terminal = (%v, %v), want a refusal", ok, err)
	}
	if buf.Len() > 0 {
		t.Errorf("a refusal printed the prompt anyway:\n%s", buf.String())
	}
}

func TestHumanBytesReadsLikeACard(t *testing.T) {
	for n, want := range map[int64]string{
		4 << 30:   "4.0 GiB",
		512 << 20: "512 MiB",
		1024:      "1024 B",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
