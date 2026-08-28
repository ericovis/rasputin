package cpio

import (
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"reflect"
	"strings"
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
	got, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Entry{
		{Name: "dev", Mode: 0o755, IsDir: true},
		{Name: "dev/console", Mode: 0o600, IsCharDev: true, Major: 5, Minor: 1},
		{Name: "init", Mode: 0o755, Data: []byte("hello")},
		{Name: "etc/empty", Mode: 0o644},
	}
	if len(got) != len(want) {
		t.Fatalf("read %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if Find(got, "init") == nil || Find(got, "nope") != nil {
		t.Error("Find did not behave")
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"truncated":  "0707",
		"bad magic":  "0707XX" + strings.Repeat("0", 104) + "x\x00\x00\x00",
		"no trailer": "",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Read(strings.NewReader(in)); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestReadRejectsBadHeaderField(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	must(t, w.WriteFile("init", 0o755, []byte("x")))
	must(t, w.Close())
	b := buf.Bytes()
	copy(b[6+6*8:6+7*8], "zzzzzzzz") // filesize field
	if _, err := Read(bytes.NewReader(b)); err == nil {
		t.Fatal("want error for a non-hex header field")
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
