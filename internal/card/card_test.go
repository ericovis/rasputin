package card

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/ericovis/rasputin/internal/mbr"
)

// image builds a zstd-compressed disk image the way a capture does: an MBR
// with a boot and a root partition, a body of exactly the bytes that table
// accounts for, and a frame checksum. used is what the table claims, which is
// the same as the body length unless a test is asking for a truncated image.
func image(t *testing.T, p2Sectors uint32, body int) []byte {
	t.Helper()
	const p1Start, p1Len = 8192, 1024
	sector := make([]byte, mbr.SectorSize)
	putEntry(sector, 0, 0x0c, p1Start, p1Len)
	putEntry(sector, 1, 0x83, p1Start+p1Len, p2Sectors)
	sector[510], sector[511] = 0x55, 0xAA

	if body == 0 {
		body = int(p1Start+p1Len+p2Sectors) * mbr.SectorSize
	}
	card := make([]byte, body)
	copy(card, sector)
	// A recognisable pattern, so a card written short or shifted by a sector
	// is not a run of zeros that happens to compare equal.
	for i := mbr.SectorSize; i < len(card); i++ {
		card[i] = byte(i % 251)
	}
	return zip(t, card)
}

func zip(t *testing.T, raw []byte) []byte {
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

func putEntry(s []byte, i int, ptype byte, start, count uint32) {
	e := s[446+i*16:]
	e[4] = ptype
	e[8], e[9], e[10], e[11] = byte(start), byte(start>>8), byte(start>>16), byte(start>>24)
	e[12], e[13], e[14], e[15] = byte(count), byte(count>>8), byte(count>>16), byte(count>>24)
}

// writeTo writes content (a zstd image) to a fresh file target and returns
// the report, the target path and the error.
func writeTo(t *testing.T, content []byte, progress func(written, total int64)) (Report, string, error) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "image.img.zst")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "card.img")
	rep, err := Write(context.Background(), src, dst, progress)
	return rep, dst, err
}

// TestWriteDecodesTheWholeImage is the command's whole job: the file on the
// other side is byte for byte what the image decodes to, including the tail,
// which the sector alignment buffers hold back until the final flush.
func TestWriteDecodesTheWholeImage(t *testing.T) {
	// 5000 sectors past a 4.5 MiB boot partition: more than one write block,
	// and not a whole number of them.
	content := image(t, 5000, 0)
	var lastTotal int64
	rep, dst, err := writeTo(t, content, func(_, total int64) { lastTotal = total })
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	want := unzip(t, content)
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the written card is %d bytes, want %d bytes equal to the image", len(got), len(want))
	}
	if rep.Bytes != int64(len(want)) {
		t.Errorf("report says %d bytes, want %d", rep.Bytes, len(want))
	}
	if rep.Device != dst {
		t.Errorf("report device = %q, want %q", rep.Device, dst)
	}
	if lastTotal != int64(len(want)) {
		t.Errorf("progress total = %d, want the partition table's %d", lastTotal, len(want))
	}
}

// TestWriteRejectsAnImageShorterThanItsTable is bake's rule applied here: an
// image that decodes to less than its partition table accounts for is a
// truncated capture, and a card written from one has half a root filesystem.
func TestWriteRejectsAnImageShorterThanItsTable(t *testing.T) {
	full := int(8192+1024+5000) * mbr.SectorSize
	_, _, err := writeTo(t, image(t, 5000, full-mbr.SectorSize), nil)
	if err == nil {
		t.Fatal("Write accepted an image shorter than its partition table")
	}
	if !strings.Contains(err.Error(), "partition table claims") {
		t.Errorf("error = %v, want it to name the mismatch", err)
	}
}

// TestWriteRefusesSomethingThatIsNotAnImage: the first sector is held back
// until it parses, so pointing the command at the wrong file costs nothing.
func TestWriteRefusesSomethingThatIsNotAnImage(t *testing.T) {
	_, dst, err := writeTo(t, zip(t, bytes.Repeat([]byte("not an image"), 1000)), nil)
	if err == nil {
		t.Fatal("Write accepted a file that is not a disk image")
	}
	info, serr := os.Stat(dst)
	if serr != nil {
		t.Fatalf("stat %s: %v", dst, serr)
	}
	if info.Size() != 0 {
		t.Errorf("%d bytes reached the target before the image was recognised", info.Size())
	}
}

// TestWriteRejectsAnImageWithoutARootPartition: a one-partition image would
// decode and verify, and boot to nothing.
func TestWriteRejectsAnImageWithoutARootPartition(t *testing.T) {
	sector := make([]byte, mbr.SectorSize)
	putEntry(sector, 0, 0x0c, 8192, 1024)
	sector[510], sector[511] = 0x55, 0xAA
	card := make([]byte, int(8192+1024)*mbr.SectorSize)
	copy(card, sector)

	_, _, err := writeTo(t, zip(t, card), nil)
	if err == nil || !strings.Contains(err.Error(), "boot and root") {
		t.Fatalf("Write on a one-partition image = %v, want it refused", err)
	}
}

// TestWriteReportsAMissingImage: the path is the operator's to fix, so the
// error has to be the plain one from the filesystem.
func TestWriteReportsAMissingImage(t *testing.T) {
	_, err := Write(context.Background(), filepath.Join(t.TempDir(), "absent.zst"), filepath.Join(t.TempDir(), "card.img"), nil)
	if err == nil || !strings.Contains(err.Error(), "absent.zst") {
		t.Fatalf("Write with no image = %v, want it to name the missing file", err)
	}
}

// TestWriteSaysHowToGetPermission: writing a disk node needs root, and a bare
// EACCES leaves the operator guessing.
func TestWriteSaysHowToGetPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: nothing refuses the write")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "image.img.zst")
	if err := os.WriteFile(src, image(t, 100, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "card.img")
	_, err := Write(context.Background(), src, dst, nil)
	if err == nil {
		t.Fatal("Write into a read-only directory succeeded")
	}
	if !strings.Contains(err.Error(), "run it again with sudo") || !strings.Contains(err.Error(), dst) {
		t.Errorf("error = %v, want the sudo hint naming the device", err)
	}
}

func TestRawPathIsTheUnbufferedNode(t *testing.T) {
	for in, want := range map[string]string{
		"/dev/disk4":       "/dev/rdisk4",
		"/dev/rdisk4":      "/dev/rdisk4",
		"/dev/disk4s1":     "/dev/rdisk4s1",
		"/tmp/card.img":    "/tmp/card.img",
		"/dev/mmcblk0":     "/dev/mmcblk0",
		"out/exported.img": "out/exported.img",
	} {
		if got := RawPath(in); got != want {
			t.Errorf("RawPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsDiskTreatsAMissingDevNodeAsADisk: a typo in a disk number must fail
// as a missing disk, not quietly create a multi-gigabyte file under /dev.
func TestIsDiskTreatsAMissingDevNodeAsADisk(t *testing.T) {
	if !IsDisk("/dev/disk99") {
		t.Error("IsDisk(/dev/disk99) = false, want a disk")
	}
	f := filepath.Join(t.TempDir(), "card.img")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if IsDisk(f) {
		t.Errorf("IsDisk(%q) = true, want a plain file", f)
	}
}

func unzip(t *testing.T, content []byte) []byte {
	t.Helper()
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	raw, err := dec.DecodeAll(content, nil)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestBufferedPathIsWhatDiskutilTakes: diskutil unmounts and ejects the disk,
// not the raw node, whichever of the two the operator named.
func TestBufferedPathIsWhatDiskutilTakes(t *testing.T) {
	for in, want := range map[string]string{
		"/dev/rdisk4":   "/dev/disk4",
		"/dev/disk4":    "/dev/disk4",
		"/tmp/card.img": "/tmp/card.img",
	} {
		if got := BufferedPath(in); got != want {
			t.Errorf("BufferedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// sectors is a target as strict as a raw disk node on macOS: anything that is
// not a whole number of sectors is EINVAL, whatever it is a sync of.
type sectors struct {
	got   []byte
	syncs int
	err   error
	// syncErr is what the medium answers fsync with, as a raw node does.
	syncErr error
}

func (s *sectors) Write(p []byte) (int, error) {
	if len(p)%mbr.SectorSize != 0 {
		s.err = fmt.Errorf("write of %d bytes is not a multiple of the %d-byte sector", len(p), mbr.SectorSize)
		return 0, s.err
	}
	s.got = append(s.got, p...)
	return len(p), nil
}

func (s *sectors) Sync() error { s.syncs++; return s.syncErr }

// TestAlignedSyncKeepsThePartialSector is the trap this buffer exists for.
// Sync is not a "the image is over" signal: the shared pipeline calls it
// every 256 MiB, with the buffer holding whatever the zstd decoder last
// returned. Flushing that would fail on /dev/rdiskN a quarter of the way
// through a card, and shift every write after it off a sector boundary.
func TestAlignedSyncKeepsThePartialSector(t *testing.T) {
	strict := &sectors{}
	a := &aligned{w: strict}

	want := make([]byte, 4*mbr.SectorSize)
	for i := range want {
		want[i] = byte(i % 251)
	}
	// Odd lengths, as a decoder hands them over, with a sync mid-stream.
	for _, chunk := range [][]byte{want[:100], want[100:1000], want[1000:1600]} {
		if _, err := a.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := a.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if strict.syncs != 1 {
		t.Errorf("the medium was synced %d times, want 1", strict.syncs)
	}
	if _, err := a.Write(want[1600:]); err != nil {
		t.Fatalf("Write after Sync: %v", err)
	}
	if err := a.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if !bytes.Equal(strict.got, want) {
		t.Errorf("the medium took %d bytes, want all %d of them", len(strict.got), len(want))
	}
}

// TestAlignedSyncTakesARawNodeThatCannotFsync: /dev/rdiskN is a character
// device, and macOS answers fsync on it with ENOTTY. The shared pipeline
// syncs every 256 MiB, so the first one killed every real card write at
// exactly that offset; the node is unbuffered and the answer means "nothing
// to flush", not "lost". Any other error must still surface.
func TestAlignedSyncTakesARawNodeThatCannotFsync(t *testing.T) {
	raw := &sectors{syncErr: syscall.ENOTTY}
	a := &aligned{w: raw}
	if _, err := a.Write(make([]byte, mbr.SectorSize)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := a.Sync(); err != nil {
		t.Errorf("Sync on a raw node = %v, want ENOTTY ignored", err)
	}
	if err := a.finish(); err != nil {
		t.Errorf("finish on a raw node = %v, want ENOTTY ignored", err)
	}
	if raw.syncs != 2 {
		t.Errorf("fsync was attempted %d times, want 2", raw.syncs)
	}

	broken := &sectors{syncErr: syscall.EIO}
	a = &aligned{w: broken}
	if err := a.Sync(); !errors.Is(err, syscall.EIO) {
		t.Errorf("Sync on a failing medium = %v, want EIO", err)
	}
}

// TestReadBackCatchesACardThatKeptNothing: a write-protected or failing card
// takes every write and keeps none of them, and the first sector is where
// that shows. Only a target that changes underneath the write proves the
// check runs at all — a file always reads back what it was given.
func TestReadBackCatchesACardThatKeptNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "card.img")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0}, mbr.SectorSize), 0o644); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0xAB}, mbr.SectorSize)

	err := readBack(path, want)
	if err == nil {
		t.Fatal("readBack accepted a target that kept none of the write")
	}
	for _, s := range []string{path, "write-protected"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error = %v, want it to mention %q", err, s)
		}
	}
	if err := readBack(path, bytes.Repeat([]byte{0}, mbr.SectorSize)); err != nil {
		t.Errorf("readBack on a target that kept the write: %v", err)
	}
}

// TestWriteReadsTheTargetBack pins the read-back to the command: the check is
// only load-bearing on hardware a test cannot have, so what a test can prove
// is that Write does read the target afterwards. A write-only target is the
// cheapest way to make that read fail.
func TestWriteReadsTheTargetBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: nothing refuses the read")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "image.img.zst")
	if err := os.WriteFile(src, image(t, 100, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "card.img")
	if err := os.WriteFile(dst, nil, 0o200); err != nil {
		t.Fatal(err)
	}

	_, err := Write(context.Background(), src, dst, nil)
	if err == nil {
		t.Fatal("Write reported success without reading the target back")
	}
	if !strings.Contains(err.Error(), "reading "+dst+" back") {
		t.Errorf("error = %v, want the read-back naming the target", err)
	}
}
