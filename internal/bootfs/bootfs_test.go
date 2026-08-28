package bootfs

import (
	"bytes"
	"path/filepath"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/mbr"
)

// newTestImage builds a small MBR disk with one FAT32 partition, laid out
// like a Raspberry Pi OS image: partition 1 starts at sector 8192.
func newTestImage(t *testing.T) string {
	t.Helper()
	const (
		sectorSize = 512
		diskSize   = 96 << 20
		start      = 8192
	)
	path := filepath.Join(t.TempDir(), "test.img")
	d, err := diskfs.Create(path, diskSize, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatalf("creating the test image: %v", err)
	}
	table := &mbr.Table{
		LogicalSectorSize:  sectorSize,
		PhysicalSectorSize: sectorSize,
		Partitions: []*mbr.Partition{{
			Index: 1,
			Type:  mbr.Fat32LBA,
			Start: start,
			Size:  diskSize/sectorSize - start,
		}},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("partitioning the test image: %v", err)
	}
	if _, err := d.CreateFilesystem(disk.FilesystemSpec{
		Partition:   BootPartition,
		FSType:      filesystem.TypeFat32,
		VolumeLabel: "bootfs",
	}); err != nil {
		t.Fatalf("creating FAT32 on the test image: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("closing the test image: %v", err)
	}
	return path
}

func TestImageWriteReadOverwriteRemove(t *testing.T) {
	path := newTestImage(t)

	img, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if img.Path() != path {
		t.Errorf("Path() = %q, want %q", img.Path(), path)
	}

	// A file large enough to span many clusters, like recovery.gz.
	big := bytes.Repeat([]byte("RECOVERY"), 90000)
	mustWrite(t, img, "recovery.gz", big)
	mustWrite(t, img, "config.txt", []byte("# long original content\n"+string(bytes.Repeat([]byte("x=1\n"), 500))))
	mustWrite(t, img, "ssh", nil)

	// Overwrite config.txt with something much shorter: the tail of the old
	// content must not survive.
	short := []byte("# short\n")
	mustWrite(t, img, "config.txt", short)

	if err := img.Remove("ssh"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := img.Remove("never-existed"); err != nil {
		t.Errorf("removing an absent file should succeed, got %v", err)
	}
	if err := img.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open: everything must still be there, and correct.
	img2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer img2.Close()

	if got := mustRead(t, img2, "recovery.gz"); !bytes.Equal(got, big) {
		t.Errorf("recovery.gz read back as %d bytes, want %d", len(got), len(big))
	}
	if got := mustRead(t, img2, "config.txt"); !bytes.Equal(got, short) {
		t.Errorf("config.txt = %q, want %q", got, short)
	}
	if img2.Exists("ssh") {
		t.Error("ssh still exists after Remove")
	}
	if !img2.Exists("recovery.gz") {
		t.Error("Exists says recovery.gz is missing")
	}

	names, err := img2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 || names[0] != "config.txt" || names[1] != "recovery.gz" {
		t.Errorf("List = %v, want [config.txt recovery.gz]", names)
	}

	info, err := img2.Stat("config.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != int64(len(short)) {
		t.Errorf("Stat size = %d, want %d", info.Size(), len(short))
	}
}

func TestReadFileIsBoundedByTheDirectoryEntry(t *testing.T) {
	// go-diskfs hands back the whole final cluster when reading to EOF, so a
	// file whose length is not a multiple of the cluster size is the case
	// that catches a regression here.
	path := newTestImage(t)
	img, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	content := bytes.Repeat([]byte("A"), 1272) // the stock config.txt length
	mustWrite(t, img, "config.txt", content)
	if got := mustRead(t, img, "config.txt"); len(got) != len(content) {
		t.Errorf("read %d bytes, want exactly %d", len(got), len(content))
	}
}

func TestOpenRejectsNonImages(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "does-not-exist.img")); err == nil {
		t.Error("Open accepted a missing file")
	}
}

func TestReadMissingFile(t *testing.T) {
	img, err := Open(newTestImage(t))
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	if _, err := img.ReadFile("absent.txt"); err == nil {
		t.Error("ReadFile accepted a missing file")
	}
}

func mustWrite(t *testing.T, img *Image, name string, data []byte) {
	t.Helper()
	if err := img.WriteFile(name, data); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}

func mustRead(t *testing.T, img *Image, name string) []byte {
	t.Helper()
	data, err := img.ReadFile(name)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", name, err)
	}
	return data
}
