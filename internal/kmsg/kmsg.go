// Package kmsg logs from the recovery agent.
//
// The agent runs as PID 1 in an initramfs with no syslog, no journal and
// often no attached terminal, so its only durable output channel is the
// kernel ring buffer: writes to /dev/kmsg show up in `dmesg` on the node and
// on the serial console. Every line is prefixed "rasputin: " so an operator
// can grep the boot log.
package kmsg

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Prefix is prepended to every line, including continuation lines.
const Prefix = "rasputin: "

// Logger writes prefixed lines to one or more sinks. It is safe for
// concurrent use; the agent logs from its download and write goroutines.
type Logger struct {
	mu    sync.Mutex
	sinks []io.Writer
}

// New returns a Logger writing to sinks. A nil or failing sink is tolerated:
// logging must never be the reason a reflash aborts.
func New(sinks ...io.Writer) *Logger {
	kept := make([]io.Writer, 0, len(sinks))
	for _, s := range sinks {
		if s != nil {
			kept = append(kept, s)
		}
	}
	return &Logger{sinks: kept}
}

// Printf formats and writes one log line.
func (l *Logger) Printf(format string, args ...any) {
	l.write(fmt.Sprintf(format, args...))
}

// Print writes one log line.
func (l *Logger) Print(msg string) { l.write(msg) }

func (l *Logger) write(msg string) {
	if l == nil {
		return
	}
	line := Format(msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.sinks {
		// Errors are deliberately ignored: /dev/kmsg rejects over-long
		// writes and can be missing entirely on a broken boot.
		_, _ = io.WriteString(s, line)
	}
}

// Format renders msg as one or more prefixed, newline-terminated lines.
// Embedded newlines are split so a multi-line message cannot smuggle an
// unprefixed line into the ring buffer.
func Format(msg string) string {
	msg = strings.TrimRight(msg, "\n")
	var b strings.Builder
	for _, line := range strings.Split(msg, "\n") {
		b.WriteString(Prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// Stdout returns a Logger that only writes to os.Stdout, for use on hosts
// (and tests) that have no kernel ring buffer.
func Stdout() *Logger { return New(os.Stdout) }
