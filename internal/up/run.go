package up

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/events"
	"github.com/ericovis/rasputin/internal/prepare"
)

// TransferTick is how often a running step samples the HTTP server for byte
// progress. It is a variable so tests do not have to wait a second.
var TransferTick = time.Second

// StepResult is what actually happened to one planned step.
type StepResult struct {
	ID       events.StepID
	Title    string
	Skipped  bool
	Reason   string
	Note     string
	Duration time.Duration
	Err      error
}

// Result is the whole run, for the summary the caller prints afterwards.
type Result struct {
	Steps []StepResult
	// Status is the health table read back *after* the run, and is nil when
	// the status step did not get to run. A pre-run probe printed as the
	// final state would describe a fleet that no longer exists.
	Status   []cluster.Status
	Duration time.Duration
}

// Failed reports whether any step failed.
func (r *Result) Failed() bool {
	for _, s := range r.Steps {
		if s.Err != nil {
			return true
		}
	}
	return false
}

// Step returns the result of one step, or nil.
func (r *Result) Step(id events.StepID) *StepResult {
	for i := range r.Steps {
		if r.Steps[i].ID == id {
			return &r.Steps[i]
		}
	}
	return nil
}

// Run executes a plan, reporting everything through sink.
//
// It stops at the first failure, and at an abort: the remaining steps are
// reported as skipped rather than attempted, because every later step depends
// on the earlier one having produced a sound artifact, and because a
// cancelled context means the operator asked for no further card to be
// touched. A stopped run leaves nodes in the retrying recovery agent, never
// half-written.
func Run(ctx context.Context, deps Deps, plan *Plan, sink events.Sink) (*Result, error) {
	if plan == nil {
		return nil, fmt.Errorf("up: Run needs a plan from NewPlan")
	}
	if err := deps.check(plan.opts); err != nil {
		return nil, err
	}
	if sink == nil {
		sink = events.Discard
	}
	if f, err := openLog(plan.opts.LogPath); err != nil {
		sink.Emit(events.Stamp(events.Event{Kind: events.Log,
			Message: fmt.Sprintf("could not open the run log: %v", err)}))
	} else if f != nil {
		defer f.Close()
		sink = events.Tee(sink, events.NewPlain(f))
	}

	r := &runner{deps: deps, plan: plan, sink: sink, nodeNames: names(plan.targets)}
	start := time.Now()
	result := &Result{}
	var failed bool

	for _, step := range plan.Steps {
		sr := StepResult{ID: step.ID, Title: step.Title, Reason: step.Reason}
		switch {
		case ctx.Err() != nil:
			// The operator pressed q, or ctrl-c. Every step from here on
			// wipes a card, so none of them starts.
			sr.Skipped, sr.Reason = true, "the run was aborted"
		case failed:
			sr.Skipped, sr.Reason = true, "a previous step failed"
		case step.Skip:
			sr.Skipped = true
		}
		if sr.Skipped {
			sink.Emit(events.Stamp(events.Event{Kind: events.StepSkipped, Step: step.ID, Message: sr.Reason}))
			result.Steps = append(result.Steps, sr)
			continue
		}

		sink.Emit(events.Stamp(events.Event{Kind: events.StepStarted, Step: step.ID, Message: step.Title}))
		stepStart := time.Now()
		sr.Note, sr.Err = r.exec(ctx, step)
		sr.Duration = time.Since(stepStart)
		if sr.Err != nil {
			failed = true
			sink.Emit(events.Stamp(events.Event{Kind: events.StepFailed, Step: step.ID, Message: sr.Err.Error()}))
		} else {
			sink.Emit(events.Stamp(events.Event{Kind: events.StepDone, Step: step.ID, Message: sr.Note}))
		}
		result.Steps = append(result.Steps, sr)
	}

	result.Duration = time.Since(start)
	result.Status = r.status
	// The abort is reported before any step error, and wrapped so the caller
	// can tell "the operator stopped this" from "a node failed": a step that
	// was cut short by the cancellation reports its own error too, and that
	// is not what the run should be judged on.
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("the run was aborted: %w", err)
	}
	for _, s := range result.Steps {
		if s.Err != nil {
			return result, fmt.Errorf("%s: %w", s.ID, s.Err)
		}
	}
	return result, nil
}

// runner holds the state one Run needs across steps.
type runner struct {
	deps      Deps
	plan      *Plan
	sink      events.Sink
	nodeNames []string

	// prepMeta is the prepare this run made, if it made one.
	prepMeta *prepare.Meta
	// status is the final probe, replacing the planning one.
	status []cluster.Status
}

func (r *runner) exec(ctx context.Context, step Step) (string, error) {
	switch step.ID {
	case events.StepProbe:
		return r.probe()
	case events.StepPrepare:
		return r.prepare(ctx)
	case events.StepAdopt:
		return r.adopt(ctx)
	case events.StepBake:
		return r.bake(ctx)
	case events.StepDryrun:
		return r.dryrun(ctx)
	case events.StepFlash:
		return r.flash(ctx)
	case events.StepStatus:
		return r.statusStep(ctx)
	}
	return "", fmt.Errorf("unknown step %q", step.ID)
}

// probe reports the pass NewPlan already made: planning is the probe, and
// re-running it here would only cost two more seconds and risk disagreeing
// with the plan the operator confirmed.
func (r *runner) probe() (string, error) {
	for _, s := range r.plan.Status {
		r.sink.Emit(events.Stamp(events.Event{Kind: events.Phase, Step: events.StepProbe, Node: s.Name,
			Message: probeLine(s)}))
	}
	return plural(len(r.plan.Status), "node") + " reachable", nil
}

func probeLine(s cluster.Status) string {
	build := s.BuildID
	if build == "" {
		build = "stock"
	}
	adopted := "not adopted"
	if s.Adopted {
		adopted = "adopted"
	}
	return fmt.Sprintf("%s, %s", build, adopted)
}

func (r *runner) prepare(ctx context.Context) (string, error) {
	meta, err := r.deps.Prepare(ctx, prepare.Options{
		// out/vanilla-custom.img is a 2.9 GB regenerable intermediate and
		// bake works from the .zst, so an unattended run does not keep it.
		RemoveImage: true,
		Log:         r.logger(events.StepPrepare),
	})
	if err != nil {
		return "", err
	}
	r.prepMeta = meta
	return "build " + meta.BuildID, nil
}

func (r *runner) adopt(ctx context.Context) (string, error) {
	r.deps.setLog(r.logger(events.StepAdopt))
	// Sequential on purpose: adoption reboots each node, and several nodes
	// down at once is exactly the state nobody can diagnose.
	for _, node := range r.plan.adoptTargets {
		res := r.deps.Adopt(ctx, node, cluster.AdoptOptions{RebootCheck: true})
		if res.Err != nil {
			return "", res.Err
		}
		// The wording matters: the display reads a node's phase for a word
		// that means it is finished with this step, so a terminal phase
		// starts with "done" or "skipped".
		r.phase(events.StepAdopt, node.Name, "done, adopted")
	}
	return "adopted " + plural(len(r.plan.adoptTargets), "node"), nil
}

func (r *runner) bake(ctx context.Context) (string, error) {
	r.deps.setLog(r.logger(events.StepBake))
	stop := r.watchTransfers(ctx, events.StepBake, r.captureID())
	defer stop()

	res, err := r.deps.Bake(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("golden build %s (%d bytes of card) in %s",
		res.Meta.BuildID, res.Meta.CardUsed, res.Duration.Round(time.Second)), nil
}

// captureID is the id the builder's capture is streamed under: bake uses the
// prepare build id, so the growing staged file can be watched.
func (r *runner) captureID() string {
	if r.prepMeta != nil {
		return r.prepMeta.BuildID
	}
	if r.plan.prepareMeta != nil {
		return r.plan.prepareMeta.BuildID
	}
	return ""
}

func (r *runner) dryrun(ctx context.Context) (string, error) {
	r.deps.setLog(r.logger(events.StepDryrun))
	stop := r.watchTransfers(ctx, events.StepDryrun, "")
	defer stop()

	img := r.plan.dryrunImg
	if img.Name == "" {
		img = cluster.VanillaImage
	}
	for _, node := range r.plan.targets {
		res := r.deps.Dryrun(ctx, img, node)
		if res.Err != nil {
			return "", res.Err
		}
		r.phase(events.StepDryrun, node.Name, "done, rehearsal passed")
	}
	return plural(len(r.plan.targets), "node") + " rehearsed", nil
}

func (r *runner) flash(ctx context.Context) (string, error) {
	// Re-read the golden metadata: a bake in this same run replaced it.
	meta, err := r.deps.ReadGoldenMeta()
	if err != nil {
		return "", err
	}
	for _, name := range r.plan.flashSkipped {
		r.phase(events.StepFlash, name, "skipped, already on golden build "+meta.BuildID)
	}

	r.deps.setLog(r.logger(events.StepFlash))
	stop := r.watchTransfers(ctx, events.StepFlash, "")
	defer stop()

	results, err := r.deps.Flash(ctx, cluster.GoldenImage, meta, r.plan.flashTargets,
		cluster.FlashOptions{Force: r.plan.opts.ForceFlash})
	if err != nil {
		return "", err
	}
	var failures []string
	var flashed, skipped int
	for _, res := range results {
		switch {
		case res.Err != nil:
			failures = append(failures, fmt.Sprintf("%s: %v", res.Node, res.Err))
			r.phase(events.StepFlash, res.Node, "FAILED: "+res.Err.Error())
		case res.Skipped:
			skipped++
			r.phase(events.StepFlash, res.Node, "skipped, already on golden build "+res.BuildID)
		default:
			flashed++
			r.phase(events.StepFlash, res.Node, "done, cloned build "+res.BuildID)
		}
	}
	if len(failures) > 0 {
		return "", fmt.Errorf("%d of %d node(s) failed to flash:\n%s",
			len(failures), len(results), strings.Join(failures, "\n"))
	}
	note := fmt.Sprintf("cloned %s", plural(flashed, "node"))
	if skipped+len(r.plan.flashSkipped) > 0 {
		note += fmt.Sprintf(", %d already current", skipped+len(r.plan.flashSkipped))
	}
	return note, nil
}

func (r *runner) statusStep(ctx context.Context) (string, error) {
	r.deps.setLog(r.logger(events.StepStatus))
	st := r.deps.Status(ctx, r.plan.targets)
	r.status = st
	var reachable int
	for _, s := range st {
		if s.Reachable {
			reachable++
		}
		r.sink.Emit(events.Stamp(events.Event{Kind: events.Phase, Step: events.StepStatus,
			Node: s.Name, Message: probeLine(s)}))
	}
	if reachable != len(st) {
		return "", fmt.Errorf("%d of %d node(s) do not answer after the run", len(st)-reachable, len(st))
	}
	return plural(reachable, "node") + " healthy", nil
}

// phase emits a sub-stage event for one node.
func (r *runner) phase(step events.StepID, node, msg string) {
	r.sink.Emit(events.Stamp(events.Event{Kind: events.Phase, Step: step, Node: node, Message: msg}))
}

// logger is the cluster package's logger, rendered into the sink as Log
// events and, where a line names a known sub-stage, as a Phase event too.
func (r *runner) logger(step events.StepID) func(format string, args ...any) {
	base := events.Logf(r.sink, step, r.nodeNames)
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		base("%s", msg)
		if phase, ok := phaseFor(msg); ok {
			r.phase(step, r.nodeIn(msg), phase)
		}
	}
}

// nodeIn returns the node a cluster log line is about, matching the
// "<node>: " prefix the cluster package writes.
func (r *runner) nodeIn(msg string) string {
	for _, n := range r.nodeNames {
		if strings.HasPrefix(msg, n+": ") {
			return n
		}
	}
	return ""
}

// phaseRules turn the cluster package's own log lines into Phase events.
//
// Matching on text is deliberate: bake and flash are the two operations that
// must not be rewritten for the sake of a progress bar, and a line that
// stops matching costs a label, never correctness. The first match wins.
var phaseRules = []struct{ contains, phase string }{
	{"reflashing with the prepared stock image", "reflashing the builder"},
	{"waiting for provisioning to finish", "provisioning"},
	// "done" would read as a terminal phase to the display (nodeStatusOf),
	// and the bake still has sealing and capturing to go.
	{": provisioned", "provisioning complete"},
	// The phrasing differs from the cluster's own line on purpose: the Log
	// event carries that line verbatim, so an identical phase would print
	// the same sentence twice in -plain output and in out/sync.log.
	{": sealing", "sealing the card"},
	{"arming a capture", "capturing the card"},
	{"waiting for the card to stream back", "capturing the card"},
	{"golden image:", "captured and verified"},
	{"waiting for the builder to come back", "the builder is coming back"},
	{"already running golden build", "already on the golden build"},
	{"arming a reflash", "downloading the image"},
	{"arming a dryrun", "rehearsing"},
	{"installing recovery.gz", "installing the recovery agent"},
	{"back up and booting through the recovery agent", "adopted"},
	{"down, waiting for it to come back", "waiting for it to come back"},
	{": rebooting", "rebooting the node"},
	{": healthy (", "verified"},
}

func phaseFor(msg string) (string, bool) {
	for _, rule := range phaseRules {
		if strings.Contains(msg, rule.contains) {
			return rule.phase, true
		}
	}
	return "", false
}

// watchTransfers emits byte progress until the returned stop is called.
//
// Two sources: the HTTP server's per-client counters (a node downloading an
// image) and, during a bake, the staged capture file growing on disk.
func (r *runner) watchTransfers(ctx context.Context, step events.StepID, captureID string) func() {
	if r.deps.Progress == nil && (captureID == "" || r.deps.StagePath == nil) {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(TransferTick)
		defer ticker.Stop()
		last := map[string]int64{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sampleTransfers(step, captureID, last)
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func (r *runner) sampleTransfers(step events.StepID, captureID string, last map[string]int64) {
	if r.deps.Progress != nil {
		for _, p := range r.deps.Progress() {
			node := r.nodeFor(p.MAC, p.IP, p.Client)
			if p.Bytes == last[node] {
				continue
			}
			last[node] = p.Bytes
			r.sink.Emit(events.Stamp(events.Event{Kind: events.Transfer, Step: step, Node: node,
				Bytes: p.Bytes, Total: p.Total, Rate: p.Rate()}))
		}
	}
	if captureID == "" || r.deps.StagePath == nil {
		return
	}
	info, err := os.Stat(r.deps.StagePath(captureID))
	if err != nil {
		return
	}
	builder := r.plan.cfg.Builder
	if info.Size() == last["capture:"+builder] {
		return
	}
	last["capture:"+builder] = info.Size()
	// Total is deliberately 0. The only number available would be the last
	// golden's compressed size, and that tracks the card's history rather
	// than this build (see CLAUDE.md) — a bar drawn from it would lie.
	r.sink.Emit(events.Stamp(events.Event{Kind: events.Transfer, Step: step, Node: builder,
		Bytes: info.Size()}))
}

// nodeFor maps a server progress record back to a configured node. The MAC
// is the identity that matters; the address is only a fallback.
func (r *runner) nodeFor(mac, ip, client string) string {
	if mac != "" {
		for _, n := range r.plan.cfg.Nodes {
			if strings.EqualFold(n.MAC, mac) {
				return n.Name
			}
		}
	}
	if ip != "" {
		for _, s := range r.plan.Status {
			if s.IP != "" && s.IP == ip {
				return s.Name
			}
		}
	}
	if client != "" {
		return client
	}
	return ip
}

// openLog appends to the run log, creating out/ if needed. A path of "-"
// disables it; an empty path means DefaultLogPath.
func openLog(path string) (*os.File, error) {
	switch path {
	case "-":
		return nil, nil
	case "":
		path = DefaultLogPath
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
