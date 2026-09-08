package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ericovis/rasputin/internal/events"
)

// syncWriter is the program's output; the renderer writes it from its own
// goroutine and the test never reads it.
type syncWriter struct {
	mu sync.Mutex
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(p), nil
}

// runInTest drives runWith through pipes instead of a terminal.
func runInTest(t *testing.T, keys []byte, run func(context.Context, events.Sink) error, opts ...tea.ProgramOption) error {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })

	started := make(chan struct{})
	wrapped := func(ctx context.Context, s events.Sink) error {
		close(started)
		return run(ctx, s)
	}
	if len(keys) > 0 {
		go func() {
			<-started
			pw.Write(keys)
		}()
	}
	opts = append(opts, tea.WithInput(pr), tea.WithOutput(&syncWriter{}), tea.WithoutSignalHandler())
	return runWith(context.Background(), "rasputin sync", testSteps, wrapped, opts...)
}

// quitKey is a program option that quits the display without cancelling the
// run, which is what an outside signal does to bubbletea.
func quitOnX() tea.ProgramOption {
	return tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		if k, ok := msg.(tea.KeyMsg); ok && k.String() == "x" {
			return tea.QuitMsg{}
		}
		return msg
	})
}

// shortGrace keeps the tests that wait out abortGrace quick.
func shortGrace(t *testing.T) {
	t.Helper()
	was := abortGrace
	abortGrace = 50 * time.Millisecond
	t.Cleanup(func() { abortGrace = was })
}

// TestRunWaitsForAnUnwindWhenTheDisplayFails: the program can die on its own
// (a recovered model panic, a terminal it cannot open) while the run is still
// writing cards. That path gets the same grace an abort gets, and the run is
// cancelled rather than left going.
func TestRunWaitsForAnUnwindWhenTheDisplayFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // tea.WithContext with a dead context: p.Run() fails immediately

	unwound := make(chan struct{})
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	err := runWith(context.Background(), "rasputin sync", testSteps,
		func(runCtx context.Context, _ events.Sink) error {
			<-runCtx.Done()
			close(unwound)
			return runCtx.Err()
		},
		tea.WithContext(ctx), tea.WithInput(pr), tea.WithOutput(&syncWriter{}), tea.WithoutSignalHandler())
	if err == nil {
		t.Fatal("a failed program returned no error")
	}
	select {
	case <-unwound:
	default:
		t.Error("Run returned while the run was still going; a step mid-flash was abandoned")
	}
}

// TestRunReportsADisplayThatQuitsWithTheRunStillGoing: an outside signal can
// quit bubbletea without cancelling anything. Returning nil there would make
// `rasputin sync` exit 0 on a bake that was killed at process exit.
func TestRunReportsADisplayThatQuitsWithTheRunStillGoing(t *testing.T) {
	shortGrace(t)
	err := runInTest(t, []byte("x"), func(ctx context.Context, s events.Sink) error {
		s.Emit(events.Event{Kind: events.StepStarted, Step: events.StepBake})
		<-time.After(10 * time.Second) // the bake is nowhere near done
		return nil
	}, quitOnX())
	if err == nil {
		t.Fatal("Run reported success although the run never finished")
	}
	if !strings.Contains(err.Error(), "still going") {
		t.Errorf("error = %v, want it to say the display left the run behind", err)
	}
}

// TestRunTurnsAPanicIntoAnError: the run goroutine is outside bubbletea's
// recovery, so a panic there would kill the process with the terminal still
// in raw mode and on the alt screen.
func TestRunTurnsAPanicIntoAnError(t *testing.T) {
	err := runInTest(t, nil, func(context.Context, events.Sink) error {
		panic("nil status in the flash result")
	})
	if err == nil || !strings.Contains(err.Error(), "nil status in the flash result") {
		t.Fatalf("Run after a panicking run = %v, want the panic reported as the error", err)
	}
}

func TestRunReturnsWhatRunReturns(t *testing.T) {
	want := errors.New("bake: capture timed out")
	err := runInTest(t, nil, func(ctx context.Context, s events.Sink) error {
		s.Emit(events.Event{Kind: events.StepFailed, Step: events.StepBake, Message: want.Error()})
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Run() = %v, want %v", err, want)
	}
}

func TestCtrlCCancelsTheContextPassedToRun(t *testing.T) {
	var sawCancel bool
	err := runInTest(t, []byte{3}, func(ctx context.Context, s events.Sink) error {
		s.Emit(events.Event{Kind: events.StepStarted, Step: events.StepBake})
		select {
		case <-ctx.Done():
			sawCancel = true
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("run was never cancelled")
		}
	})
	if !sawCancel {
		t.Fatal("run's context was not cancelled by ctrl-c")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
}

func TestRunSurvivesEventsSentAfterTheProgramQuits(t *testing.T) {
	// A step that keeps emitting while it unwinds must not deadlock on a sink
	// whose program is already gone.
	done := make(chan struct{})
	err := runInTest(t, []byte("q"), func(ctx context.Context, s events.Sink) error {
		<-ctx.Done()
		go func() {
			defer close(done)
			for i := 0; i < 50; i++ {
				s.Emit(events.Event{Kind: events.Log, Step: events.StepBake, Message: "unwinding"})
			}
		}()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emitting after the program quit blocked forever")
	}
}
