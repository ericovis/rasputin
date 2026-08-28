package mbr

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// stockPi builds a boot sector matching the stock Raspberry Pi OS layout:
// p1 FAT32 LBA (type 0x0c) at sector 8192 for 512 MiB, p2 Linux (0x83)
// immediately after for ~2 GiB.
func stockPi() []byte {
	const (
		p1Start = 8192
		p1Len   = 1048576 // 512 MiB in sectors
		p2Start = p1Start + p1Len
		p2Len   = 4194304 // 2 GiB in sectors
	)
	s := make([]byte, SectorSize)
	binary.LittleEndian.PutUint32(s[440:444], 0x9730496b)
	putEntry(s, 0, 0x0c, p1Start, p1Len)
	putEntry(s, 1, 0x83, p2Start, p2Len)
	s[510], s[511] = 0x55, 0xAA
	return s
}

func putEntry(s []byte, i int, ptype byte, start, count uint32) {
	e := s[tableOffset+i*entrySize:]
	e[0] = 0x00 // not bootable
	e[4] = ptype
	binary.LittleEndian.PutUint32(e[8:12], start)
	binary.LittleEndian.PutUint32(e[12:16], count)
}

func TestParseStockLayout(t *testing.T) {
	tbl, err := Parse(stockPi())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tbl.DiskID != 0x9730496b {
		t.Errorf("DiskID = %#x", tbl.DiskID)
	}
	if len(tbl.Partitions) != 2 {
		t.Fatalf("partitions = %d, want 2", len(tbl.Partitions))
	}
	p1, p2 := tbl.Partition(1), tbl.Partition(2)
	if p1.Type != 0x0c || p1.StartBytes() != 8192*512 || p1.LengthBytes() != 512<<20 {
		t.Errorf("p1 = %+v", *p1)
	}
	if p2.Type != 0x83 || p2.StartBytes() != p1.EndBytes() {
		t.Errorf("p2 = %+v, should start where p1 ends (%d)", *p2, p1.EndBytes())
	}
	if got, want := tbl.UsedBytes(), p2.EndBytes(); got != want {
		t.Errorf("UsedBytes = %d, want %d", got, want)
	}
	if tbl.Partition(3) != nil {
		t.Error("Partition(3) should be nil")
	}
}

func TestUsedBytesFromReaderAt(t *testing.T) {
	// A sparse 32 GB "card" whose table only claims the first ~2.5 GiB.
	sector := stockPi()
	disk := bytes.NewReader(append(sector, make([]byte, 4096)...))
	got, err := UsedBytes(disk)
	if err != nil {
		t.Fatalf("UsedBytes: %v", err)
	}
	if want := int64((8192 + 1048576 + 4194304)) * SectorSize; got != want {
		t.Errorf("UsedBytes = %d, want %d", got, want)
	}
}

func TestUsedBytesTakesMaximumNotLastEntry(t *testing.T) {
	// Entries out of order: slot 1 ends later than slot 2.
	s := make([]byte, SectorSize)
	putEntry(s, 0, 0x83, 100000, 10000)
	putEntry(s, 1, 0x0c, 8192, 2048)
	s[510], s[511] = 0x55, 0xAA
	tbl, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := tbl.UsedBytes(), int64(110000)*SectorSize; got != want {
		t.Errorf("UsedBytes = %d, want %d", got, want)
	}
}

func TestParseRejections(t *testing.T) {
	noSig := stockPi()
	noSig[511] = 0x00

	gpt := make([]byte, SectorSize)
	putEntry(gpt, 0, typeGPTProtective, 1, 0xFFFFFFFF)
	gpt[510], gpt[511] = 0x55, 0xAA

	empty := make([]byte, SectorSize)
	empty[510], empty[511] = 0x55, 0xAA

	// A zero-length entry with a non-zero type is stale, not a partition.
	stale := make([]byte, SectorSize)
	putEntry(stale, 0, 0x83, 2048, 0)
	stale[510], stale[511] = 0x55, 0xAA

	cases := []struct {
		name, want string
		sector     []byte
	}{
		{"short", "short sector", make([]byte, 100)},
		{"bad signature", "bad signature", noSig},
		{"gpt", "GPT", gpt},
		{"empty table", "no partitions", empty},
		{"zero-length entry", "no partitions", stale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.sector)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestReadShortDisk(t *testing.T) {
	if _, err := UsedBytes(bytes.NewReader(make([]byte, 10))); err == nil {
		t.Fatal("want error reading sector 0 of a 10-byte disk")
	}
}
