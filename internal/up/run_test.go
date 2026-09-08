package up

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/events"
	"github.com/ericovis/rasputin/internal/prepare"
	"github.com/ericovis/rasputin/internal/server"
)

// runPlan plans and runs in one go, recording every event.
func (f *fake) run(t *testing.T, opts Options) (*Result, *events.Recorder, error) {
	t.Helper()
	opts.LogPath = filepath.Join(t.TempDir(), "sync.log")
	p, err := NewPlan(context.Background(), f.deps(), opts)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	rec := &events.Recorder{}
	res, err := Run(context.Background(), f.deps(), p, rec)
	return res, rec, err
}

// trace renders the step-level events as "kind:step", which is what the
// order assertions are about.
func trace(rec *events.Recorder) string {
	var out []string
	for _, e := range rec.Events() {
		switch e.Kind {
		case events.StepStarted, events.StepDone, events.StepSkipped, events.StepFailed:
			out = append(out, e.Kind.String()+":"+string(e.Step))
		}
	}
	return strings.Join(out, " ")
}

// TestRunNothingToDo: an up on a cluster already in the configured state
// must not touch a single node.
func TestRunNothingToDo(t *testing.T) {
	f := newFake(t)
	res, rec, err := f.run(t, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.prepared) != 0 || f.bakes != 0 || len(f.flashed) != 0 || len(f.adopted) != 0 {
		t.Errorf("a no-op run did work: prepared=%d bakes=%d flashed=%v adopted=%v",
			len(f.prepared), f.bakes, f.flashed, f.adopted)
	}
	const want = "started:probe done:probe skipped:prepare skipped:adopt skipped:bake skipped:flash started:status done:status"
	if got := trace(rec); got != want {
		t.Errorf("events =\n%s\nwant\n%s", got, want)
	}
	if res.Failed() {
		t.Error("a clean run reports a failure")
	}
	if s := res.Step(events.StepFlash); !s.Skipped || !strings.Contains(s.Reason, "already runs golden build") {
		t.Errorf("flash result = %+v, want a skip explaining every node is current", s)
	}
	// Once for the plan, once for the closing health table — a third probe
	// would mean the run re-decided something the operator already saw.
	if f.statusCalls != 2 {
		t.Errorf("the cluster was probed %d times, want 2 (plan, then status)", f.statusCalls)
	}
}

// TestRunStopsWhenPrepareFails: everything downstream is built from the
// prepared image, so a broken prepare must not reach a node.
func TestRunStopsWhenPrepareFails(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new"
	f.prepareErr = errors.New("cache/raspios.img: no space left on device")

	res, rec, err := f.run(t, Options{})
	if err == nil {
		t.Fatal("Run reported success after a failed prepare")
	}
	if f.bakes != 0 || len(f.flashed) != 0 {
		t.Errorf("work continued after a failed prepare: bakes=%d flashed=%v", f.bakes, f.flashed)
	}
	if !strings.Contains(trace(rec), "failed:prepare skipped:adopt") {
		t.Errorf("events = %q, want the run to stop at prepare", trace(rec))
	}
	if res.Step(events.StepBake).Reason != "a previous step failed" {
		t.Errorf("bake result = %+v, want it skipped because prepare failed", res.Step(events.StepBake))
	}
}

// TestRunStopsAtAnAbort: q and ctrl-c cancel the context, and the README
// promises that is safe. It is only safe if no further step starts.
func TestRunStopsAtAnAbort(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new" // prepare, bake and flash all have work to do
	p, err := NewPlan(context.Background(), f.deps(), Options{LogPath: "-"})
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := &events.Recorder{}
	res, err := Run(ctx, f.deps(), p, rec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run on a cancelled context = %v, want a context.Canceled the caller can recognise", err)
	}
	if len(f.prepared) != 0 || f.bakes != 0 || len(f.flashed) != 0 || len(f.adopted) != 0 {
		t.Errorf("an aborted run did work: prepared=%d bakes=%d flashed=%v adopted=%v",
			len(f.prepared), f.bakes, f.flashed, f.adopted)
	}
	if got := trace(rec); strings.Contains(got, "started:") {
		t.Errorf("events = %q, want every step skipped", got)
	}
	if s := res.Step(events.StepBake); !s.Skipped || s.Reason != "the run was aborted" {
		t.Errorf("bake result = %+v, want it skipped as aborted", s)
	}
	if res.Status != nil {
		t.Error("an aborted run reports a health table; the probe it would show is the pre-run one")
	}
}

// TestRunAbortBeatsAStepError: a step cut short by the cancellation reports
// its own error, but the run is an abort, not a failure — the caller decides
// between "the operator stopped this" and "a node broke" on that.
func TestRunAbortBeatsAStepError(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new"
	f.prepareErr = errors.New("copying: context canceled")
	p, err := NewPlan(context.Background(), f.deps(), Options{LogPath: "-"})
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// The step runs, then the abort lands while it is unwinding.
	deps := f.deps()
	prepareFn := deps.Prepare
	deps.Prepare = func(ctx context.Context, opts prepare.Options) (*prepare.Meta, error) {
		cancel()
		return prepareFn(ctx, opts)
	}
	defer cancel()

	if _, err := Run(ctx, deps, p, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want an error that reads as an abort", err)
	}
	if f.bakes != 0 || len(f.flashed) != 0 {
		t.Errorf("work continued past the abort: bakes=%d flashed=%v", f.bakes, f.flashed)
	}
}

func TestRunRebuildsEverything(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new"
	res, rec, err := f.run(t, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.prepared) != 1 {
		t.Fatalf("prepare ran %d times, want once", len(f.prepared))
	}
	// CLAUDE.md: out/vanilla-custom.img is the first thing to delete, and an
	// unattended `sync` is exactly where it would otherwise pile up.
	if !f.prepared[0].RemoveImage {
		t.Error("up prepared without RemoveImage; the 2.9 GB intermediate would be left behind")
	}
	if f.bakes != 1 {
		t.Errorf("bake ran %d times, want once", f.bakes)
	}
	want := []string{"rasputin002", "rasputin003", "rasputin004"}
	if strings.Join(f.flashed, " ") != strings.Join(want, " ") {
		t.Errorf("flashed %v, want %v", f.flashed, want)
	}
	if f.flashOpts.Force {
		t.Error("flash was forced without -force-flash")
	}
	const wantTrace = "started:probe done:probe started:prepare done:prepare skipped:adopt started:bake done:bake started:flash done:flash started:status done:status"
	if got := trace(rec); got != wantTrace {
		t.Errorf("events =\n%s\nwant\n%s", got, wantTrace)
	}
	if note := res.Step(events.StepFlash).Note; !strings.Contains(note, "3 nodes") {
		t.Errorf("flash note = %q, want it to count the cloned nodes", note)
	}
}

// TestRunFlashesTheGoldenJustBaked guards the ordering bug that matters
// most: flash must read golden.json *after* the bake replaced it, not the
// build id that was on disk when the plan was made.
func TestRunFlashesTheGoldenJustBaked(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new"
	res, _, err := f.run(t, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.golden.BuildID != "build-2" {
		t.Fatalf("the fake baked build %s, want build-2", f.golden.BuildID)
	}
	if note := res.Step(events.StepBake).Note; !strings.Contains(note, "build-2") {
		t.Errorf("bake note = %q, want the new build id", note)
	}
}

func TestRunForceFlash(t *testing.T) {
	f := newFake(t)
	_, _, err := f.run(t, Options{ForceFlash: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.flashed) != 4 {
		t.Errorf("flashed %v, want all four nodes with -force-flash", f.flashed)
	}
	if !f.flashOpts.Force {
		t.Error("cluster.FlashOptions.Force was not set; the nodes would skip themselves")
	}
}

func TestRunAdoptsSequentially(t *testing.T) {
	f := newFake(t)
	f.node("rasputin002").Adopted = false
	f.node("rasputin004").Adopted = false
	_, rec, err := f.run(t, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(f.adopted, " "); got != "rasputin002 rasputin004" {
		t.Errorf("adopted %q, want the two unadopted nodes in config order", got)
	}
	// The cluster's own log line must reach the sink, tagged with its node.
	var tagged bool
	for _, e := range rec.Of(events.Log) {
		if e.Node == "rasputin002" && strings.Contains(e.Message, "installing recovery.gz") {
			tagged = true
		}
	}
	if !tagged {
		t.Error("no Log event carried rasputin002's adopt line; the TUI would show untagged noise")
	}
}

// TestRunStopsAtTheFirstFailure: a bake that fails must not be followed by a
// flash from a golden image nobody verified.
func TestRunStopsAtTheFirstFailure(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new"
	f.bakeErr = errors.New("the capture did not complete within 45m0s")

	res, rec, err := f.run(t, Options{})
	if err == nil {
		t.Fatal("Run reported success after a failed bake")
	}
	if !strings.Contains(err.Error(), "bake:") {
		t.Errorf("error = %q, want it to name the failed step", err)
	}
	if len(f.flashed) != 0 {
		t.Errorf("flashed %v after a failed bake", f.flashed)
	}
	if !res.Failed() {
		t.Error("Result.Failed is false after a failed bake")
	}
	flash := res.Step(events.StepFlash)
	if !flash.Skipped || flash.Reason != "a previous step failed" {
		t.Errorf("flash result = %+v, want skipped because the bake failed", flash)
	}
	const want = "started:probe done:probe started:prepare done:prepare skipped:adopt started:bake failed:bake skipped:flash skipped:status"
	if got := trace(rec); got != want {
		t.Errorf("events =\n%s\nwant\n%s", got, want)
	}
}

func TestRunReportsFlashFailuresPerNode(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new"
	f.flashFailures = map[string]bool{"rasputin003": true}

	res, rec, err := f.run(t, Options{})
	if err == nil {
		t.Fatal("Run reported success although a node failed to flash")
	}
	if !strings.Contains(err.Error(), "rasputin003") {
		t.Errorf("error = %q, want it to name the failed node", err)
	}
	if res.Step(events.StepStatus).Reason != "a previous step failed" {
		t.Error("status ran after a failed flash")
	}
	var sawPhase bool
	for _, e := range rec.Of(events.Phase) {
		if e.Node == "rasputin003" && strings.Contains(e.Message, "FAILED") {
			sawPhase = true
		}
	}
	if !sawPhase {
		t.Error("no Phase event marked rasputin003 as failed")
	}
}

func TestRunRehearsesEveryNode(t *testing.T) {
	f := newFake(t)
	_, rec, err := f.run(t, Options{Rehearse: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.rehearsed) != 4 {
		t.Errorf("rehearsed %v, want all four nodes", f.rehearsed)
	}
	if got := trace(rec); !strings.Contains(got, "done:dryrun skipped:flash") {
		t.Errorf("events = %q, want the dryrun to precede the flash", got)
	}
}

// TestRunWritesTheLog: the TUI only shows a tail, so the whole run has to
// survive somewhere.
func TestRunWritesTheLog(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new"
	path := filepath.Join(t.TempDir(), "sync.log")
	p, err := NewPlan(context.Background(), f.deps(), Options{LogPath: path})
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if _, err := Run(context.Background(), f.deps(), p, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the run log: %v", err)
	}
	for _, want := range []string{"==> prepare", "<== bake", "--- adopt: skipped"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the run log is missing %q:\n%s", want, data)
		}
	}
}

// TestPhasesDoNotEchoTheirLogLine: the raw cluster line is emitted as a Log
// event and the derived phase as a Phase event, so a phase worded exactly
// like the line prints the same sentence twice in -plain and in out/sync.log.
func TestPhasesDoNotEchoTheirLogLine(t *testing.T) {
	for _, rule := range phaseRules {
		if strings.TrimPrefix(rule.contains, ": ") == rule.phase {
			t.Errorf("the phase for %q is the line itself (%q); word it differently",
				rule.contains, rule.phase)
		}
	}
}

func TestPhaseFor(t *testing.T) {
	cases := []struct{ line, want string }{
		{"rasputin001: reflashing with the prepared stock image from http://…", "reflashing the builder"},
		{"rasputin001: sealing", "sealing the card"},
		{"rasputin002: rebooting", "rebooting the node"},
		{"rasputin001: arming a capture to http://192.168.0.10:8080/c/x", "capturing the card"},
		{"rasputin001: waiting for the builder to come back", "the builder is coming back"},
		{"rasputin002: arming a reflash from http://…", "downloading the image"},
		{"rasputin002: already running golden build build-1 — skipping", "already on the golden build"},
		{"rasputin002: healthy (rasputin002, build build-1)", "verified"},
		{"compressed 2900000000 bytes to 700000000 (24.1%)", ""},
	}
	for _, tc := range cases {
		got, ok := phaseFor(tc.line)
		if tc.want == "" {
			if ok {
				t.Errorf("phaseFor(%q) = %q, want no phase", tc.line, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("phaseFor(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// TestTransferEventsNameTheNode: progress arrives keyed by MAC, and the UI
// needs a node name — the MAC is the identity, the address only a fallback.
func TestTransferEventsNameTheNode(t *testing.T) {
	f := newFake(t)
	p := f.plan(t, Options{})
	rec := &events.Recorder{}
	r := &runner{deps: f.deps(), plan: p, sink: rec, nodeNames: names(p.targets)}

	f.setProgress(
		server.Progress{MAC: "B8:27:EB:00:00:02", IP: "192.168.0.12", Client: "B8:27:EB:00:00:02", Bytes: 5 << 20, Total: 100 << 20},
		server.Progress{IP: "192.168.0.13", Client: "192.168.0.13", Bytes: 1 << 20},
	)
	last := map[string]int64{}
	r.sampleTransfers(events.StepFlash, "", last)

	got := rec.Of(events.Transfer)
	if len(got) != 2 {
		t.Fatalf("emitted %d transfer events, want 2", len(got))
	}
	if got[0].Node != "rasputin002" || got[0].Bytes != 5<<20 {
		t.Errorf("first transfer = %+v, want rasputin002 at 5 MiB", got[0])
	}
	if got[1].Node != "rasputin003" {
		t.Errorf("second transfer = %q, want rasputin003 resolved from its address", got[1].Node)
	}
	// An unchanged byte count must not re-emit: the bars would flicker and
	// the log would fill with nothing.
	rec2 := &events.Recorder{}
	r.sink = rec2
	r.sampleTransfers(events.StepFlash, "", last)
	if n := len(rec2.Of(events.Transfer)); n != 0 {
		t.Errorf("emitted %d transfer events for unchanged progress, want 0", n)
	}
}

// TestCaptureTransferWatchesTheStagedFile: during a bake nothing is
// downloading, so the only sign of progress is the capture growing on disk.
func TestCaptureTransferWatchesTheStagedFile(t *testing.T) {
	f := newFake(t)
	stage := filepath.Join(t.TempDir(), "incoming-build-1.zst.tmp")
	if err := os.WriteFile(stage, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	p := f.plan(t, Options{})
	deps := f.deps()
	deps.Progress = nil
	deps.StagePath = func(string) string { return stage }

	rec := &events.Recorder{}
	r := &runner{deps: deps, plan: p, sink: rec, nodeNames: names(p.targets)}
	r.sampleTransfers(events.StepBake, "build-1", map[string]int64{})

	got := rec.Of(events.Transfer)
	if len(got) != 1 {
		t.Fatalf("emitted %d transfer events, want 1 for the growing capture", len(got))
	}
	if got[0].Node != f.cfg.Builder || got[0].Bytes != 1<<20 {
		t.Errorf("capture transfer = %+v, want the builder at 1 MiB", got[0])
	}
	// The only available total would be the last golden's compressed size,
	// which tracks the card's history rather than this build (CLAUDE.md).
	if got[0].Total != 0 {
		t.Errorf("capture transfer claims a total of %d; it cannot know one", got[0].Total)
	}
}

func TestRunRejectsAnIncompletePlan(t *testing.T) {
	if _, err := Run(context.Background(), Deps{}, nil, nil); err == nil {
		t.Error("Run accepted a nil plan")
	}
	if _, err := NewPlan(context.Background(), Deps{}, Options{}); err == nil {
		t.Error("NewPlan accepted empty deps")
	}
}

// TestWatchTransfersPollsWhileAStepRuns exercises the polling itself: a long
// step must animate, and the poller must stop with the step.
func TestWatchTransfersPollsWhileAStepRuns(t *testing.T) {
	old := TransferTick
	TransferTick = time.Millisecond
	defer func() { TransferTick = old }()

	f := newFake(t)
	f.fingerprint = "fp-new"
	deps := f.deps()
	baked := make(chan struct{})
	deps.Bake = func(ctx context.Context) (*cluster.BakeResult, error) {
		f.setProgress(server.Progress{MAC: f.cfg.Nodes[0].MAC, Bytes: 700 << 20, Total: 900 << 20})
		<-baked
		return &cluster.BakeResult{Meta: &cluster.GoldenMeta{BuildID: "build-2"}}, nil
	}

	p, err := NewPlan(context.Background(), deps, Options{LogPath: "-"})
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	rec := &events.Recorder{}
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), deps, p, rec)
		done <- err
	}()

	deadline := time.After(5 * time.Second)
	for {
		if len(rec.Of(events.Transfer)) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no Transfer event while the bake was running")
		case <-time.After(time.Millisecond):
		}
	}
	close(baked)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := rec.Of(events.Transfer)[0]
	if got.Node != "rasputin001" || got.Total != 900<<20 {
		t.Errorf("transfer = %+v, want the builder with a known total", got)
	}
	// Nothing may be emitted after the run returned: the sink belongs to the
	// caller, and a TUI that has quit would be written to from a stray
	// goroutine.
	before := len(rec.Events())
	f.setProgress(server.Progress{MAC: f.cfg.Nodes[0].MAC, Bytes: 800 << 20, Total: 900 << 20})
	time.Sleep(20 * time.Millisecond)
	if after := len(rec.Events()); after != before {
		t.Errorf("%d more events arrived after Run returned; the poller outlived its step", after-before)
	}
}
