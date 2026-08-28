package cpio

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"testing"
)

// consoleEntry is the exact byte layout documented in plan/FACTS.md for a
// dev/console character device written as the first entry of an archive:
// magic, then ino=1 mode=0x2180 uid=0 gid=0 nlink=1 mtime=0 filesize=0
// devmaj=0 devmin=0 rdevmaj=5 rdevmin=1 namesize=12 check=0, then
// "dev/console\0" padded with 2 NULs so 110+12 rounds up to 124.
const consoleEntry = "070701" +
	"00000001" + "00002180" + "00000000" + "00000000" +
	"00000001" + "00000000" + "00000000" + "00000000" +
	"00000000" + "00000005" + "00000001" + "0000000c" +
	"00000000" + "dev/console\x00\x00\x00"

func TestConsoleEntryGolden(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteCharDev("dev/console", 0o600, 5, 1); err != nil {
		t.Fatalf("WriteCharDev: %v", err)
	}
	if got, want := buf.String(), consoleEntry; got != want {
		t.Errorf("console entry mismatch\ngot:  %s\nwant: %s",
			hex.EncodeToString([]byte(got)), hex.EncodeToString([]byte(want)))
	}
	if buf.Len()%4 != 0 {
		t.Errorf("entry length %d is not 4-byte aligned", buf.Len())
	}
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	// "init" is deliberately 4 bytes long and its data 5, so both the name
	// padding (0 bytes) and the data padding (3 bytes) paths get exercised.
	must(t, w.WriteDir("dev"))
	must(t, w.WriteCharDev("dev/console", 0o600, 5, 1))
	must(t, w.WriteFile("init", 0o755, []byte("hello")))
	must(t, w.WriteFile("etc/empty", 0o644, nil))
	must(t, w.Close())

	if buf.Len()%4 != 0 {
		t.Fatalf("archive length %d is not 4-byte aligned", buf.Len())
	}
	got, err := readAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := []entry{
		{"dev", modeDir | 0o755, 0, 0, nil},
		{"dev/console", modeChar | 0o600, 5, 1, nil},
		{"init", modeFile | 0o755, 0, 0, []byte("hello")},
		{"etc/empty", modeFile | 0o644, 0, 0, nil},
	}
	if len(got) != len(want) {
		t.Fatalf("read %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].name != want[i].name || got[i].mode != want[i].mode ||
			got[i].rmaj != want[i].rmaj || got[i].rmin != want[i].rmin ||
			!bytes.Equal(got[i].data, want[i].data) {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestNameValidation(t *testing.T) {
	w := NewWriter(io.Discard)
	if err := w.WriteFile("/absolute", 0o644, nil); err == nil {
		t.Error("absolute name accepted, want error")
	}
	if err := w.WriteFile("", 0o644, nil); err == nil {
		t.Error("empty name accepted, want error")
	}
}

func TestWriteAfterClose(t *testing.T) {
	w := NewWriter(io.Discard)
	must(t, w.Close())
	if err := w.WriteFile("late", 0o644, nil); err == nil {
		t.Error("write after Close accepted, want error")
	}
}

func TestWriteErrorPropagates(t *testing.T) {
	w := NewWriter(failWriter{})
	if err := w.WriteFile("x", 0o644, []byte("y")); err == nil {
		t.Fatal("want error from failing writer")
	}
	if err := w.WriteFile("x2", 0o644, nil); err == nil {
		t.Error("want sticky error on subsequent write")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// --- minimal newc reader, used only by these tests --------------------------

type entry struct {
	name       string
	mode       uint32
	rmaj, rmin uint32
	data       []byte
}

func readAll(r *bytes.Reader) ([]entry, error) {
	var out []entry
	pos := 0
	read := func(n int) ([]byte, error) {
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		pos += n
		return b, nil
	}
	skipPad := func() error {
		if p := pos % 4; p != 0 {
			b, err := read(4 - p)
			if err != nil {
				return err
			}
			for _, c := range b {
				if c != 0 {
					return fmt.Errorf("non-NUL padding byte %#x", c)
				}
			}
		}
		return nil
	}
	for {
		h, err := read(headerSize)
		if err != nil {
			return nil, err
		}
		if string(h[:6]) != magic {
			return nil, fmt.Errorf("bad magic %q", h[:6])
		}
		field := func(i int) uint32 {
			v, _ := strconv.ParseUint(string(h[6+i*8:6+(i+1)*8]), 16, 32)
			return uint32(v)
		}
		mode, filesize, rmaj, rmin, namesize := field(1), field(6), field(9), field(10), field(11)
		nameBuf, err := read(int(namesize))
		if err != nil {
			return nil, err
		}
		if nameBuf[namesize-1] != 0 {
			return nil, fmt.Errorf("name %q is not NUL-terminated", nameBuf)
		}
		name := string(nameBuf[:namesize-1])
		if err := skipPad(); err != nil {
			return nil, err
		}
		if name == trailer {
			return out, nil
		}
		var data []byte
		if filesize > 0 {
			if data, err = read(int(filesize)); err != nil {
				return nil, err
			}
			if err := skipPad(); err != nil {
				return nil, err
			}
		}
		out = append(out, entry{name, mode, rmaj, rmin, data})
	}
}
