// Package events is the contract between the orchestration layer and whatever
// is showing progress to the operator: the TUI, a plain line logger, a test.
//
// Producers emit Events into a Sink; they never know how the event is drawn.
// The cluster package keeps its `func(format string, args ...any)` logger, so
// Logf adapts a Sink into one of those.
package events

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Kind says what an Event reports.
type Kind int

const (
	// StepStarted: a step began. Message may carry a one-line description.
	StepStarted Kind = iota
	// StepSkipped: the step was not needed; Message says why.
	StepSkipped
	// StepDone: the step finished successfully; Message may summarise it.
	StepDone
	// StepFailed: the step failed; Message is the error.
	StepFailed
	// Log: a free-form line from inside a step (Node may be set).
	Log
	// Transfer: byte progress for one node (Node, Bytes, Total, Rate).
	// Total <= 0 means the total is unknown.
	Transfer
	// Phase: a running step entered a sub-stage (Message names it, Node may
	// scope it). It lets a long step like bake show where it is without
	// pretending to know a byte count.
	Phase
)

// String is the lower-case name, for logs and tests.
func (k Kind) String() string {
	switch k {
	case StepStarted:
		return "started"
	case StepSkipped:
		return "skipped"
	case StepDone:
		return "done"
	case StepFailed:
		return "failed"
	case Log:
		return "log"
	case Transfer:
		return "transfer"
	case Phase:
		return "phase"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// StepID names one step of the end-to-end run. The orchestrator defines the
// set; the UI treats them as opaque labels.
type StepID string

// The steps `rasputin up` knows about, in order.
const (
	StepPrepare StepID = "prepare"
	StepAdopt   StepID = "adopt"
	StepBake    StepID = "bake"
	StepDryrun  StepID = "dryrun"
	StepFlash   StepID = "flash"
	StepStatus  StepID = "status"
)

// Event is one thing that happened.
type Event struct {
	Kind    Kind
	Step    StepID
	Node    string // empty when the event is not about one node
	Message string
	Bytes   int64   // Transfer only
	Total   int64   // Transfer only; <= 0 when unknown
	Rate    float64 // Transfer only; bytes per second
	Time    time.Time
}

// Sink receives events. Implementations must be safe for concurrent use:
// steps run goroutines per node.
type Sink interface {
	Emit(Event)
}

// Func adapts a plain function into a Sink.
type Func func(Event)

// Emit calls f.
func (f Func) Emit(e Event) { f(e) }

// Discard drops everything.
var Discard Sink = Func(func(Event) {})

// Stamp fills in Time if the producer left it zero.
func Stamp(e Event) Event {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	return e
}

// Logf adapts a Sink into the logger the cluster package takes. Every line
// becomes a Log event under step. If the line starts with "<node>: " for a
// node in names, Node is set and the prefix kept in Message unchanged, so a
// plain sink still prints exactly what the cluster package said.
func Logf(s Sink, step StepID, names []string) func(format string, args ...any) {
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		e := Event{Kind: Log, Step: step, Message: msg}
		for _, n := range names {
			if strings.HasPrefix(msg, n+": ") {
				e.Node = n
				break
			}
		}
		s.Emit(Stamp(e))
	}
}

// Plain renders events as text lines, one per event, for terminals that are
// not a TTY and for tests. It is safe for concurrent use.
type Plain struct {
	mu sync.Mutex
	w  io.Writer
	// ShowTransfers prints Transfer events too; off by default because a
	// polled transfer is noisy in a log file.
	ShowTransfers bool
}

// NewPlain returns a Plain sink writing to w.
func NewPlain(w io.Writer) *Plain { return &Plain{w: w} }

// Emit writes the event as one line.
func (p *Plain) Emit(e Event) {
	if e.Kind == Transfer && !p.ShowTransfers {
		return
	}
	line := Format(e)
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.w, line)
}

// Format renders an event as a single line without a trailing newline.
func Format(e Event) string {
	switch e.Kind {
	case StepStarted:
		return fmt.Sprintf("==> %s%s", e.Step, suffix(e.Message))
	case StepSkipped:
		return fmt.Sprintf("--- %s: skipped%s", e.Step, suffix(e.Message))
	case StepDone:
		return fmt.Sprintf("<== %s: done%s", e.Step, suffix(e.Message))
	case StepFailed:
		return fmt.Sprintf("!!! %s: FAILED%s", e.Step, suffix(e.Message))
	case Transfer:
		who := e.Node
		if who == "" {
			who = string(e.Step)
		}
		if e.Total <= 0 {
			return fmt.Sprintf("%s: %d MiB", who, e.Bytes>>20)
		}
		pct := 100 * float64(e.Bytes) / float64(e.Total)
		return fmt.Sprintf("%s: %d/%d MiB (%.0f%%, %.1f MB/s)", who, e.Bytes>>20, e.Total>>20, pct, e.Rate/1e6)
	case Phase:
		if e.Node != "" {
			return fmt.Sprintf("%s: %s", e.Node, e.Message)
		}
		return fmt.Sprintf("%s: %s", e.Step, e.Message)
	default:
		return e.Message
	}
}

func suffix(msg string) string {
	if msg == "" {
		return ""
	}
	return " — " + msg
}

// Tee fans one event out to several sinks, in order.
func Tee(sinks ...Sink) Sink {
	return Func(func(e Event) {
		for _, s := range sinks {
			if s != nil {
				s.Emit(e)
			}
		}
	})
}

// Recorder keeps every event, for tests.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// Emit appends.
func (r *Recorder) Emit(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Events returns a copy of what was recorded so far.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// Of returns the recorded events of one kind.
func (r *Recorder) Of(k Kind) []Event {
	var out []Event
	for _, e := range r.Events() {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}
