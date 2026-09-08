package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/events"
	"github.com/ericovis/rasputin/internal/server"
	"github.com/ericovis/rasputin/internal/tui"
	"github.com/ericovis/rasputin/internal/up"
)

// runSync is the one command the whole CLI exists to make unnecessary to
// spell out: prepare, adopt, bake, flash and status, each skipped when its
// output is already what rasputin.yaml describes.
func runSync(cfg *config.Config, out *output, args []string) error {
	opts, flags, err := parseSyncFlags(cfg, out, args)
	if err != nil {
		return err
	}

	// The sudo prompt has to happen before the display takes the terminal.
	pw, err := sudoPassword(cfg)
	if err != nil {
		return err
	}

	// One indirection for every logger. c.Serve copies c.Log into the HTTP
	// server at Serve time, so repointing c.Log alone would leave the
	// server printing "served golden.img.zst to …" straight through the
	// TUI. Everything goes through sw, and the run repoints sw.
	sw := &logSwitch{}
	c, err := cluster.NewWithSudo(cfg, pw, sw.logf)
	if err != nil {
		return err
	}
	// Planning is read-only and needs no HTTP server; -plan must work while
	// another rasputin holds the port (a sync in progress, say).
	var srv *server.Server
	if !flags.PlanOnly {
		if srv, err = c.Serve(); err != nil {
			return err
		}
		defer srv.Close()
	}

	deps := up.NewDeps(c, srv)
	setLog := deps.SetLog
	deps.SetLog = func(logf func(format string, args ...any)) {
		if setLog != nil {
			setLog(logf)
		}
		sw.set(logf)
	}

	// Ctrl-C outside the TUI must unwind the same way it does inside it:
	// cancel the run, let the step return, leave the node in the agent.
	// SIGTERM is here too because the display no longer handles signals
	// itself: one owner, and it is the one that can cancel the run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return executeSync(ctx, deps, opts, syncUI{Out: out, In: os.Stdin, Plain: flags.Plain, PlanOnly: flags.PlanOnly})
}

// syncFlags are the switches that shape the command rather than the run.
type syncFlags struct {
	// Plain prints one line per event instead of drawing.
	Plain bool
	// PlanOnly stops after printing the plan; nothing is touched.
	PlanOnly bool
}

// parseSyncFlags turns the command line into up.Options. It is separate from
// runSync so the flag contract can be tested without a cluster.
func parseSyncFlags(cfg *config.Config, out *output, args []string) (up.Options, syncFlags, error) {
	fs := out.flagSet("sync")
	force := fs.Bool("force", false, "shorthand for -force-prepare -force-bake -force-flash")
	forcePrepare := fs.Bool("force-prepare", false, "rebuild the recovery agent and the stock image even if they match the config")
	forceBake := fs.Bool("force-bake", false, "bake a new golden image even if the current one matches the prepared image")
	forceFlash := fs.Bool("force-flash", false, "reflash every node, including one already on the golden build")
	rehearse := fs.Bool("rehearse", false, "dryrun every node before flashing, to prove the pipeline without writing a card")
	yes := fs.Bool("yes", false, "do not ask before wiping a node")
	plain := fs.Bool("plain", false, "print one line per event instead of drawing the progress display")
	planOnly := fs.Bool("plan", false, "probe, print the plan, and stop without touching anything")
	logPath := fs.String("log", up.DefaultLogPath, `append every event to this file ("-" for none)`)
	fs.Usage = func() {
		out.printfErr("usage: rasputin sync [flags]\n\n"+
			"Brings the whole cluster to the state %s describes: prepares the\n"+
			"image, adopts what is not adopted, bakes the golden image when it is\n"+
			"stale and clones it to every node that is not already running it.\n"+
			"Each step is skipped when its output is already current.\n\n"+
			"The plan is printed first, and a plan that wipes a card asks before\n"+
			"it starts (-plan stops there; -yes answers it). Aborting with q or\n"+
			"ctrl-c is safe: a node left mid-run stays in the recovery agent,\n"+
			"which keeps retrying.\n\nflags:\n", cfg.Path)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return up.Options{}, syncFlags{}, err
	}
	if fs.NArg() > 0 {
		return up.Options{}, syncFlags{}, fmt.Errorf(
			"sync takes no arguments; it acts on every node in %s (flags come first)", cfg.Path)
	}
	return up.Options{
		ForcePrepare: *force || *forcePrepare,
		ForceBake:    *force || *forceBake,
		ForceFlash:   *force || *forceFlash,
		Rehearse:     *rehearse,
		Yes:          *yes,
		LogPath:      *logPath,
	}, syncFlags{Plain: *plain, PlanOnly: *planOnly}, nil
}

// syncUI is where `sync` talks to the operator: the summary, the confirmation
// and the choice between the drawn display, plain lines and JSON.
type syncUI struct {
	Out *output
	In  *os.File
	// Plain forces line output. A run without a terminal is always plain,
	// and so is JSON mode.
	Plain bool
	// PlanOnly stops after the plan.
	PlanOnly bool
	// Confirm replaces the terminal prompt in tests.
	Confirm func() (bool, error)
}

// executeSync is `sync` once the cluster exists: plan, show, confirm, run,
// report. It takes up.Deps rather than a Cluster so the whole flow, this
// command's real logic, is testable without a Pi.
func executeSync(ctx context.Context, deps up.Deps, opts up.Options, ui syncUI) error {
	out := ui.Out
	plan, err := up.NewPlan(ctx, deps, opts)
	if err != nil {
		return err
	}
	if out.json {
		out.emit(toPlanJSON(plan))
	}
	out.printf("%s", plan.Summary())

	if ui.PlanOnly {
		out.printf("\n-plan given: nothing was touched\n")
		return out.result("sync", struct {
			PlanOnly bool `json:"plan_only"`
		}{true}, nil)
	}

	if plan.NeedsConfirmation() && !opts.Yes {
		ok, err := ui.confirm()
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("cancelled; nothing was touched")
		}
	}
	out.printf("\n")

	// The run happens in the TUI's goroutine, and an abort can return from
	// tui.Run before it has unwound, so the result is handed over under a
	// lock rather than by a bare assignment.
	var mu sync.Mutex
	var result *up.Result
	do := func(ctx context.Context, sink events.Sink) error {
		res, err := up.Run(ctx, deps, plan, sink)
		mu.Lock()
		result = res
		mu.Unlock()
		return err
	}

	var runErr error
	switch {
	case out.json:
		runErr = do(ctx, events.Func(func(e events.Event) { out.emit(toJSONEvent(e)) }))
	case ui.plainOutput():
		runErr = do(ctx, events.NewPlain(out.w))
	default:
		runErr = tui.Run(ctx, "rasputin sync · cluster "+plan.Cluster, tuiSteps(plan.Steps), do)
	}

	mu.Lock()
	res := result
	mu.Unlock()
	if res != nil {
		out.printf("\n%s", resultTable(res))
		// Only the status step fills Result.Status. A run that stopped before
		// it must not end with the pre-run probe printed as if it were the
		// state the cluster was left in.
		if len(res.Status) > 0 {
			out.printf("\n%s", cluster.StatusTable(res.Status))
		}
	}
	aborted := errors.Is(runErr, context.Canceled)
	if aborted {
		// Aborting is not a failed step: the recovery agent keeps retrying,
		// and the next `sync` picks up exactly where this one stopped.
		runErr = errors.New("aborted; the nodes are still in the recovery agent, nothing is half-written")
	}
	return out.result("sync", toSyncResultJSON(res, aborted, opts.LogPath), runErr)
}

// plainOutput reports whether to print lines instead of drawing. No
// terminal means plain whatever the flag says.
func (u syncUI) plainOutput() bool { return u.Plain || u.Out.json || !tui.IsTerminal() }

// confirm asks before the first card is wiped. JSON mode never asks: a
// program cannot answer a prompt, and must say -yes to mean it.
func (u syncUI) confirm() (bool, error) {
	if u.Confirm != nil {
		return u.Confirm()
	}
	if u.Out.json {
		return false, errors.New(
			"this plan wipes at least one node and -json never prompts: re-run with -yes, or -plan to only look")
	}
	if u.In == nil || !term.IsTerminal(int(u.In.Fd())) {
		return false, errors.New(
			"this plan wipes at least one node and there is no terminal to confirm on: re-run with -yes")
	}
	u.Out.printf("\nProceed? [y/N] ")
	line, err := bufio.NewReader(u.In).ReadString('\n')
	if err != nil && line == "" {
		return false, fmt.Errorf("reading the answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// tuiSteps hands the plan to the display. The two Step types are copies of
// each other on purpose: tui must not import up.
func tuiSteps(steps []up.Step) []tui.Step {
	out := make([]tui.Step, len(steps))
	for i, s := range steps {
		out[i] = tui.Step{
			ID:       s.ID,
			Title:    s.Title,
			Skip:     s.Skip,
			Reason:   s.Reason,
			Nodes:    s.Nodes,
			Estimate: s.Estimate,
		}
	}
	return out
}

// resultTable is what survives in the scrollback after the display is gone.
func resultTable(r *up.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-9s %-7s %-8s %s\n", "STEP", "RESULT", "TIME", "DETAIL")
	for _, s := range r.Steps {
		verdict, detail := "OK", s.Note
		took := s.Duration.Round(time.Second).String()
		switch {
		case s.Err != nil:
			verdict, detail = "FAIL", firstLine(s.Err.Error())
		case s.Skipped:
			verdict, detail, took = "SKIP", s.Reason, "-"
		case detail == "":
			detail = s.Title
		}
		fmt.Fprintf(&b, "%-9s %-7s %-8s %s\n", s.ID, verdict, took, detail)
	}
	fmt.Fprintf(&b, "\ntotal %s\n", r.Duration.Round(time.Second))
	return b.String()
}

// firstLine keeps a multi-line error (flash reports one line per node) to
// one row of the table; the whole thing is in out/sync.log.
func firstLine(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		return msg[:i] + " …"
	}
	return msg
}

// logSwitch is a logger that can be repointed while the things holding it
// keep the same function value.
type logSwitch struct {
	mu sync.Mutex
	to func(format string, args ...any)
}

// logf is the function every logger in the run is built from. Until set is
// called it is silent, which keeps the planning probe from scribbling over
// the plan summary.
func (l *logSwitch) logf(format string, args ...any) {
	l.mu.Lock()
	to := l.to
	l.mu.Unlock()
	if to != nil {
		to(format, args...)
	}
}

func (l *logSwitch) set(to func(format string, args ...any)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.to = to
}
