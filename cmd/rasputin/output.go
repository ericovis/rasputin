package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// output is where a command talks to whoever ran it: a person at a terminal,
// or a program.
//
// In text mode it prints what it always printed. In JSON mode (-json) stdout
// carries nothing but newline-delimited JSON objects, one per line, each with
// a "type" field, and the last one is always {"type":"result", ...} with "ok"
// and, on failure, "error". Everything meant only for a person — usage text,
// the sudo prompt — goes to stderr. The manual (cmd/rasputin/MANUAL.md)
// documents every object; keep the two in step.
type output struct {
	// json selects the machine form. Root -json, the per-command -json and
	// the pre-scan in main all bind here.
	json bool
	w    io.Writer // stdout
	errw io.Writer // stderr: usage text, prompts
	mu   sync.Mutex
	done bool // the result object has been written
}

func stdOutput() *output { return &output{w: os.Stdout, errw: os.Stderr} }

// flagSet is how every command declares its flags: usage text goes to
// stderr, so it never pollutes JSON on stdout, and -json is always defined.
func (o *output) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(o.errw)
	fs.BoolVar(&o.json, "json", o.json,
		"write newline-delimited JSON to stdout instead of text (see `rasputin manual`)")
	return fs
}

// hasJSONFlag reports whether -json appears among args, so main can answer
// in JSON even when the failure happens before the command parses its
// flags (a config that does not load, say). It stops at "--".
func hasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		switch strings.TrimLeft(a, "-") {
		case "json", "json=true", "json=1", "json=t", "json=T", "json=TRUE", "json=True":
			if strings.HasPrefix(a, "-") {
				return true
			}
		}
	}
	return false
}

// printf is text-only output. In JSON mode it is dropped, because nothing
// that is not JSON may reach stdout there; anything a machine needs must be
// in the result object instead.
func (o *output) printf(format string, a ...any) {
	if o.json {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprintf(o.w, format, a...)
}

// printfErr writes to stderr: usage text and anything else meant for a
// person's eyes only. It is never JSON and never on stdout.
func (o *output) printfErr(format string, a ...any) {
	fmt.Fprintf(o.errw, format, a...)
}

// logf is the progress logger commands hand to the cluster package: a line
// of text, or a {"type":"log"} object. Safe for concurrent use; flashes log
// from one goroutine per node.
func (o *output) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if o.json {
		o.emit(logLine{Type: "log", Message: msg, Time: time.Now()})
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprintln(o.w, msg)
}

type logLine struct {
	Type    string    `json:"type"`
	Message string    `json:"message"`
	Time    time.Time `json:"time"`
}

// emit writes one JSON object as one line. It is only called in JSON mode.
func (o *output) emit(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		// A value that cannot be marshalled is a programming error; say so
		// on the stream rather than silently dropping the line.
		raw, _ = json.Marshal(logLine{Type: "log", Message: "internal: " + err.Error(), Time: time.Now()})
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.w.Write(raw)
	o.w.Write([]byte{'\n'})
}

// result ends a command. In text mode it does nothing but hand err back. In
// JSON mode it writes the final {"type":"result"} object: the envelope
// (type, command, ok, error) followed by the fields of v, which must be a
// struct or map that marshals to an object; nil means no extra fields.
//
// It returns err unchanged so callers can write `return o.result(...)`.
// A second call is a no-op, which lets main add the envelope for errors
// that escaped before a command reached its own result.
func (o *output) result(command string, v any, err error) error {
	if !o.json {
		return err
	}
	o.mu.Lock()
	done := o.done
	o.done = true
	o.mu.Unlock()
	if done {
		return err
	}

	env := envelope{Type: "result", Command: command, OK: err == nil}
	if err != nil {
		env.Error = err.Error()
	}
	head, _ := json.Marshal(env)
	body := []byte("{}")
	if v != nil {
		b, merr := json.Marshal(v)
		switch {
		case merr != nil:
			b, _ = json.Marshal(map[string]string{"marshal_error": merr.Error()})
		case len(b) == 0 || b[0] != '{':
			b, _ = json.Marshal(map[string]json.RawMessage{"data": b})
		}
		body = b
	}
	line := head
	if len(body) > 2 {
		// Splice the two objects: "{...envelope" + "," + "fields...}".
		line = append(append(head[:len(head)-1], ','), body[1:]...)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.w.Write(line)
	o.w.Write([]byte{'\n'})
	return err
}

type envelope struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// seconds renders a duration the way every JSON object here does: as a
// number of seconds, so consumers never parse "1m30s".
func seconds(d time.Duration) float64 { return d.Seconds() }
