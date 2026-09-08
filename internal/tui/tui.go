// Package tui draws `rasputin sync` while it runs. It is the only package
// allowed to import bubbletea: everything it knows arrives as events.Event,
// so the orchestrator never learns how progress is shown, and the plain
// renderer stays a drop-in replacement for it.
package tui

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/ericovis/rasputin/internal/events"
)

// Step mirrors up.Step. It is copied rather than imported so tui stays dumb
// (and cannot grow a dependency on the orchestrator).
type Step struct {
	ID       events.StepID
	Title    string
	Skip     bool
	Reason   string
	Nodes    []string
	Estimate time.Duration
}

// abortGrace is how long Run waits for run to unwind after the user aborts
// before reporting the abort anyway. A step that ignores its context must not
// hold the terminal hostage. It is a variable so tests do not wait ten
// seconds to prove what happens when the grace runs out.
var abortGrace = 10 * time.Second

// Run draws the TUI while run executes. run is called in a goroutine with a
// Sink that feeds the display; its context is cancelled when the user presses
// q or ctrl-c, and run is expected to return promptly then. Run returns run's
// error, the abort error if the user quit, or an error of its own if the
// display went away with the run still going — never nil for a run that did
// not finish. It prints nothing itself once the program is gone: the caller
// owns the scrollback.
func Run(ctx context.Context, title string, steps []Step, run func(ctx context.Context, sink events.Sink) error) error {
	// One owner for signals: the caller's signal.NotifyContext cancels ctx,
	// run unwinds, and the program quits through its own shutdown so the
	// terminal is restored. Left to bubbletea, a SIGTERM would quit the
	// display while the bake carried on with nobody watching it.
	return runWith(ctx, title, steps, run, tea.WithAltScreen(), tea.WithoutSignalHandler())
}

// runWith is Run with the program options spelled out, so tests can drive it
// through pipes instead of a terminal.
func runWith(ctx context.Context, title string, steps []Step, run func(context.Context, events.Sink) error, opts ...tea.ProgramOption) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	p := tea.NewProgram(newModel(title, steps, cancel), opts...)

	errc := make(chan error, 1)
	go func() {
		// A panic in the orchestrator would otherwise unwind the runtime with
		// the terminal still in raw mode and the alt screen still up:
		// bubbletea only recovers its own goroutines. Reported as the run's
		// error, it leaves through the program's normal shutdown instead.
		defer func() {
			if v := recover(); v != nil {
				err := fmt.Errorf("panic: %v", v)
				errc <- err
				p.Send(doneMsg{err: err})
			}
		}()
		err := run(runCtx, sink{p})
		errc <- err
		// Send unblocks on its own once the program has exited.
		p.Send(doneMsg{err: err})
	}()

	if _, err := p.Run(); err != nil {
		// The display is gone (a recovered model panic, a terminal that
		// could not be opened). run is still writing cards, so it gets the
		// same unwinding grace an abort gets before the error is reported.
		cancel()
		waitFor(errc)
		return err
	}
	if runCtx.Err() != nil {
		// Aborted: let run unwind so nothing is left mid-write, then say so.
		waitFor(errc)
		return runCtx.Err()
	}
	select {
	case err := <-errc:
		return err
	case <-time.After(abortGrace):
		// The program quit while the run is still going — nothing cancelled
		// it, so this is not an abort and it is certainly not success. Say
		// what happened rather than let `sync` exit 0 on a half-done bake.
		cancel()
		waitFor(errc)
		return fmt.Errorf("the display exited while the run was still going; "+
			"the nodes are still in the recovery agent, see %s", logHint)
	}
}

// logHint names where the rest of the story is. tui must not import up, so
// the path is spelled out rather than shared.
const logHint = "out/sync.log"

// waitFor gives run its unwinding grace. A step that ignores its context must
// not hold the terminal hostage, so the wait is bounded.
func waitFor(errc <-chan error) {
	select {
	case <-errc:
	case <-time.After(abortGrace):
	}
}

// sink hands events to the running program.
type sink struct{ p *tea.Program }

// Emit is safe for concurrent use and becomes a no-op once the program exits.
func (s sink) Emit(e events.Event) { s.p.Send(eventMsg(events.Stamp(e))) }

// IsTerminal reports whether stdout can carry the TUI. The caller falls back
// to events.NewPlain when it cannot.
func IsTerminal() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
