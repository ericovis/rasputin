// Package mbr reads the DOS partition table at the start of a disk image.
//
// The recovery agent uses it to answer one question: how many bytes at the
// front of the SD card are actually in use? Capturing a golden image means
// streaming only that prefix, which turns a 32 GB card into a few GB.
package mbr

import (
	"encoding/binary"
	"fmt"
	"io"
)

// SectorSize is the logical sector size assumed throughout; Raspberry Pi SD
// cards and the official images are all 512-byte sectors.
const SectorSize = 512

const (
	tableOffset = 446
	entrySize   = 16
	numEntries  = 4

	typeGPTProtective = 0xEE
)

// Partition is one non-empty MBR partition entry.
type Partition struct {
	Index      int    // 1-based, matching /dev/mmcblk0pN
	Type       byte   // partition type byte, e.g. 0x0c FAT32 LBA, 0x83 Linux
	StartLBA   uint32 // first sector
	SectorsLen uint32 // length in sectors
}

// StartBytes is the partition's byte offset from the start of the disk.
func (p Partition) StartBytes() int64 { return int64(p.StartLBA) * SectorSize }

// LengthBytes is the partition's size in bytes.
func (p Partition) LengthBytes() int64 { return int64(p.SectorsLen) * SectorSize }

// EndBytes is the byte offset just past the partition.
func (p Partition) EndBytes() int64 { return p.StartBytes() + p.LengthBytes() }

// Table is a parsed MBR.
type Table struct {
	DiskID     uint32 // the NT disk signature; cloning it keeps root=PARTUUID= valid
	Partitions []Partition
}

// Read parses sector 0 of r.
func Read(r io.ReaderAt) (*Table, error) {
	sector := make([]byte, SectorSize)
	if _, err := r.ReadAt(sector, 0); err != nil {
		return nil, fmt.Errorf("mbr: read sector 0: %w", err)
	}
	return Parse(sector)
}

// Parse parses a 512-byte boot sector.
func Parse(sector []byte) (*Table, error) {
	if len(sector) < SectorSize {
		return nil, fmt.Errorf("mbr: short sector: %d bytes", len(sector))
	}
	if sector[510] != 0x55 || sector[511] != 0xAA {
		return nil, fmt.Errorf("mbr: bad signature %#02x%#02x, not a DOS partition table", sector[510], sector[511])
	}
	t := &Table{DiskID: binary.LittleEndian.Uint32(sector[440:444])}
	for i := 0; i < numEntries; i++ {
		e := sector[tableOffset+i*entrySize:]
		ptype := e[4]
		if ptype == typeGPTProtective {
			return nil, fmt.Errorf("mbr: GPT protective partition found; GPT disks are not supported")
		}
		start := binary.LittleEndian.Uint32(e[8:12])
		count := binary.LittleEndian.Uint32(e[12:16])
		if ptype == 0 || count == 0 {
			continue
		}
		t.Partitions = append(t.Partitions, Partition{
			Index:      i + 1,
			Type:       ptype,
			StartLBA:   start,
			SectorsLen: count,
		})
	}
	if len(t.Partitions) == 0 {
		return nil, fmt.Errorf("mbr: no partitions in table")
	}
	return t, nil
}

// UsedBytes returns the byte offset just past the last used sector: the
// amount of the disk worth capturing. It is the maximum of every non-empty
// partition's end, so it also covers the gap-free case where p2 is last.
func UsedBytes(r io.ReaderAt) (int64, error) {
	t, err := Read(r)
	if err != nil {
		return 0, err
	}
	return t.UsedBytes(), nil
}

// UsedBytes is the parsed-table form of the package-level UsedBytes.
func (t *Table) UsedBytes() int64 {
	var max int64
	for _, p := range t.Partitions {
		if end := p.EndBytes(); end > max {
			max = end
		}
	}
	return max
}

// Partition returns the 1-based partition, or nil if absent.
func (t *Table) Partition(index int) *Partition {
	for i := range t.Partitions {
		if t.Partitions[i].Index == index {
			return &t.Partitions[i]
		}
	}
	return nil
}
