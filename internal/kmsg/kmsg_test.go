package kmsg

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestFormatPrefixesEveryLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "rasputin: hello\n"},
		{"hello\n", "rasputin: hello\n"},
		{"a\nb", "rasputin: a\nrasputin: b\n"},
		{"", "rasputin: \n"},
	}
	for _, tc := range cases {
		if got := Format(tc.in); got != tc.want {
			t.Errorf("Format(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLoggerWritesToAllSinks(t *testing.T) {
	var a, b bytes.Buffer
	l := New(&a, nil, &b)
	l.Printf("writing %d of %d bytes", 1, 2)
	l.Print("done")
	want := "rasputin: writing 1 of 2 bytes\nrasputin: done\n"
	if a.String() != want || b.String() != want {
		t.Errorf("a=%q b=%q, want both %q", a.String(), b.String(), want)
	}
}

func TestLoggerIgnoresSinkErrors(t *testing.T) {
	var ok bytes.Buffer
	l := New(failWriter{}, &ok)
	l.Print("still logged")
	if !strings.Contains(ok.String(), "still logged") {
		t.Errorf("healthy sink got %q", ok.String())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestLoggerConcurrent(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); l.Printf("line %d", i) }(i)
	}
	wg.Wait()
	if n := strings.Count(buf.String(), Prefix); n != 50 {
		t.Errorf("got %d prefixed lines, want 50", n)
	}
}

func TestNilLoggerIsSafe(t *testing.T) {
	var l *Logger
	l.Print("no panic")
}
