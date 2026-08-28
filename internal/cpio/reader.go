package cpio

import (
	"fmt"
	"io"
	"os"
	"strconv"
)

// Entry is one archive member returned by Read.
type Entry struct {
	Name      string
	Mode      os.FileMode // permission bits only
	IsDir     bool
	IsCharDev bool
	Major     uint32
	Minor     uint32
	Data      []byte
}

// Read parses a whole newc archive. It exists so builds can verify what they
// just packed — an initramfs that is wrong is only discovered on a node that
// no longer boots, which is exactly the trip we are trying to avoid.
func Read(r io.Reader) ([]Entry, error) {
	var (
		out []Entry
		pos int
	)
	read := func(n int) ([]byte, error) {
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		pos += n
		return b, nil
	}
	skipPad := func() error {
		p := pos % 4
		if p == 0 {
			return nil
		}
		b, err := read(4 - p)
		if err != nil {
			return err
		}
		for _, c := range b {
			if c != 0 {
				return fmt.Errorf("cpio: non-NUL padding byte %#x", c)
			}
		}
		return nil
	}
	for {
		h, err := read(headerSize)
		if err != nil {
			return nil, fmt.Errorf("cpio: read header: %w", err)
		}
		if string(h[:6]) != magic {
			return nil, fmt.Errorf("cpio: bad magic %q", h[:6])
		}
		field := func(i int) (uint32, error) {
			v, err := strconv.ParseUint(string(h[6+i*8:6+(i+1)*8]), 16, 32)
			if err != nil {
				return 0, fmt.Errorf("cpio: bad header field %d: %w", i, err)
			}
			return uint32(v), nil
		}
		mode, err := field(1)
		if err != nil {
			return nil, err
		}
		filesize, err := field(6)
		if err != nil {
			return nil, err
		}
		major, err := field(9)
		if err != nil {
			return nil, err
		}
		minor, err := field(10)
		if err != nil {
			return nil, err
		}
		namesize, err := field(11)
		if err != nil {
			return nil, err
		}
		if namesize == 0 {
			return nil, fmt.Errorf("cpio: zero-length name")
		}
		nameBuf, err := read(int(namesize))
		if err != nil {
			return nil, err
		}
		if nameBuf[namesize-1] != 0 {
			return nil, fmt.Errorf("cpio: name %q is not NUL-terminated", nameBuf)
		}
		e := Entry{
			Name:      string(nameBuf[:namesize-1]),
			Mode:      os.FileMode(mode & 0o7777),
			IsDir:     mode&modeFmt == modeDir,
			IsCharDev: mode&modeFmt == modeChar,
			Major:     major,
			Minor:     minor,
		}
		if err := skipPad(); err != nil {
			return nil, err
		}
		if e.Name == trailer {
			return out, nil
		}
		if filesize > 0 {
			if e.Data, err = read(int(filesize)); err != nil {
				return nil, err
			}
			if err := skipPad(); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
}

// Find returns the entry with the given name, or nil.
func Find(entries []Entry, name string) *Entry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}
