package up

import (
	"context"
	"strings"
	"testing"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/prepare"
)

// TestPlanNothingToDo is the case the whole command exists for: a cluster
// already in the configured state must plan a run that touches nothing.
func TestPlanNothingToDo(t *testing.T) {
	f := newFake(t)
	p := f.plan(t, Options{})

	for _, id := range []string{"prepare", "adopt", "bake", "flash"} {
		if s := stepByID(t, p, id); !s.Skip {
			t.Errorf("%s is planned to run (%s); nothing changed, so it should be skipped", id, s.Reason)
		}
	}
	if p.NeedsConfirmation() {
		t.Errorf("a no-op plan asks for confirmation; wipes = %v", p.Wipes())
	}
	if got := stepByID(t, p, "flash").Reason; !strings.Contains(got, "build-1") {
		t.Errorf("flash skip reason = %q, want it to name the golden build", got)
	}
}

func TestPlanConfigChanged(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new" // the operator edited rasputin.yaml
	p := f.plan(t, Options{})

	prep := stepByID(t, p, "prepare")
	if prep.Skip {
		t.Fatal("prepare is skipped although the config fingerprint moved")
	}
	if !strings.Contains(prep.Reason, "rasputin.yaml changed") {
		t.Errorf("prepare reason = %q, want it to name the changed config", prep.Reason)
	}
	if bake := stepByID(t, p, "bake"); bake.Skip {
		t.Fatal("bake is skipped although the image is being rebuilt")
	}

	// The bake leaves the builder on the new build, so only the others clone.
	flash := stepByID(t, p, "flash")
	want := []string{"rasputin002", "rasputin003", "rasputin004"}
	if strings.Join(flash.Nodes, " ") != strings.Join(want, " ") {
		t.Errorf("flash nodes = %v, want %v (the builder is already on the new build)", flash.Nodes, want)
	}
	wipes := strings.Join(p.Wipes(), ", ")
	const wantWipes = "rasputin001 (bake), rasputin002 (flash), rasputin003 (flash), rasputin004 (flash)"
	if wipes != wantWipes {
		t.Errorf("wipes = %q, want %q", wipes, wantWipes)
	}
	if !p.NeedsConfirmation() {
		t.Error("a plan that wipes four nodes does not ask for confirmation")
	}
	if s := p.Summary(); !strings.Contains(s, "WILL WIPE") {
		t.Errorf("the summary does not warn about wiping:\n%s", s)
	}
}

func TestPlanDecisions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fake)
		opts  Options
		// wantRun lists the steps that must not be skipped.
		wantRun []string
		// wantFlash is the exact set of nodes the flash step targets.
		wantFlash []string
	}{
		{
			// The bake stamps the prepare's build id into the golden, so
			// re-baking an unchanged prepare produces the id the fleet is
			// already running: nothing to clone, and nothing to announce as
			// about to be wiped.
			name:      "a missing golden image with a current prepare",
			setup:     func(f *fake) { f.golden = nil },
			wantRun:   []string{"bake"},
			wantFlash: nil,
		},
		{
			name:      "a missing golden image and a fleet a build behind",
			setup:     func(f *fake) { f.golden = nil; f.staleNodes("build-0") },
			wantRun:   []string{"bake", "flash"},
			wantFlash: []string{"rasputin002", "rasputin003", "rasputin004"},
		},
		{
			name:      "the golden file is gone but its meta is not",
			setup:     func(f *fake) { f.missing[cluster.GoldenImage.Path] = true },
			wantRun:   []string{"bake"},
			wantFlash: nil,
		},
		{
			name:      "nothing prepared yet",
			setup:     func(f *fake) { f.prep = nil },
			wantRun:   []string{"prepare", "bake", "flash"},
			wantFlash: []string{"rasputin002", "rasputin003", "rasputin004"},
		},
		{
			name:      "the prepared zst was deleted",
			setup:     func(f *fake) { f.missing[prepare.ImageZstPath] = true },
			wantRun:   []string{"prepare", "bake", "flash"},
			wantFlash: []string{"rasputin002", "rasputin003", "rasputin004"},
		},
		{
			name:      "a prepare from before fingerprinting",
			setup:     func(f *fake) { f.prep.Fingerprint = "" },
			wantRun:   []string{"prepare", "bake", "flash"},
			wantFlash: []string{"rasputin002", "rasputin003", "rasputin004"},
		},
		{
			name:      "one node still runs a stock system",
			setup:     func(f *fake) { f.node("rasputin003").BuildID = "" },
			wantRun:   []string{"flash"},
			wantFlash: []string{"rasputin003"},
		},
		{
			name:      "one node was never adopted",
			setup:     func(f *fake) { f.node("rasputin004").Adopted = false },
			wantRun:   []string{"adopt"},
			wantFlash: nil,
		},
		{
			name:      "-force-flash reflashes current nodes",
			opts:      Options{ForceFlash: true},
			wantRun:   []string{"flash"},
			wantFlash: []string{"rasputin001", "rasputin002", "rasputin003", "rasputin004"},
		},
		{
			name:      "-force-bake rebakes without re-preparing",
			opts:      Options{ForceBake: true},
			wantRun:   []string{"bake"},
			wantFlash: nil,
		},
		{
			name:      "-force-prepare drags bake and flash with it",
			opts:      Options{ForcePrepare: true},
			wantRun:   []string{"prepare", "bake", "flash"},
			wantFlash: []string{"rasputin002", "rasputin003", "rasputin004"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			p := f.plan(t, tc.opts)

			run := map[string]bool{}
			for _, s := range p.Steps {
				if !s.Skip {
					run[string(s.ID)] = true
				}
			}
			for _, id := range tc.wantRun {
				if !run[id] {
					t.Errorf("%s is skipped (%s), want it to run", id, stepByID(t, p, id).Reason)
				}
			}
			flash := stepByID(t, p, "flash")
			if strings.Join(flash.Nodes, " ") != strings.Join(tc.wantFlash, " ") {
				t.Errorf("flash nodes = %v, want %v", flash.Nodes, tc.wantFlash)
			}
			if len(tc.wantFlash) == 0 && !flash.Skip {
				t.Error("flash has no targets but is not marked skipped")
			}
		})
	}
}

// TestPlanPairsProbesByIdentity: Deps.Status promises no order, and this is
// the decision that picks which card gets wiped. Never trust a position any
// more than a hostname.
func TestPlanPairsProbesByIdentity(t *testing.T) {
	f := newFake(t)
	f.node("rasputin003").BuildID = "build-0" // the one node that must flash
	deps := f.deps()
	probe := deps.Status
	deps.Status = func(ctx context.Context, targets []config.Node) []cluster.Status {
		out := probe(ctx, targets)
		reversed := make([]cluster.Status, len(out))
		for i, s := range out {
			reversed[len(out)-1-i] = s
		}
		return reversed
	}

	p, err := NewPlan(context.Background(), deps, Options{LogPath: "-"})
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	flash := stepByID(t, p, "flash")
	if want := []string{"rasputin003"}; strings.Join(flash.Nodes, " ") != strings.Join(want, " ") {
		t.Errorf("flash nodes = %v, want %v; the probe was paired by position", flash.Nodes, want)
	}
}

// TestPlanRefusesAProbeMissingANode: a probe that does not describe every
// target cannot be paired at all, and guessing would wipe the wrong card.
func TestPlanRefusesAProbeMissingANode(t *testing.T) {
	f := newFake(t)
	deps := f.deps()
	probe := deps.Status
	deps.Status = func(ctx context.Context, targets []config.Node) []cluster.Status {
		return probe(ctx, targets)[:2]
	}

	_, err := NewPlan(context.Background(), deps, Options{LogPath: "-"})
	if err == nil {
		t.Fatal("NewPlan accepted a probe that skipped two nodes")
	}
	if !strings.Contains(err.Error(), "rasputin003") {
		t.Errorf("error %q does not name the node with no probe result", err)
	}
}

// TestPlanRefusesUnreachableNodes: planning contacts every node, and an
// absent one has to stop the run before anything is written.
func TestPlanRefusesUnreachableNodes(t *testing.T) {
	f := newFake(t)
	down := f.node("rasputin003")
	down.Reachable, down.Err = false, context.DeadlineExceeded

	_, err := NewPlan(context.Background(), f.deps(), Options{LogPath: "-"})
	if err == nil {
		t.Fatal("NewPlan accepted an unreachable node")
	}
	for _, want := range []string{"rasputin003", "nothing has been touched", "rasputin.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestPlanRehearseInsertsDryrunBeforeFlash(t *testing.T) {
	f := newFake(t)
	f.fingerprint = "fp-new"
	p := f.plan(t, Options{Rehearse: true})

	got := strings.Join(stepIDs(p), " ")
	const want = "probe prepare adopt bake dryrun flash status"
	if got != want {
		t.Errorf("steps = %q, want %q", got, want)
	}
	if dry := stepByID(t, p, "dryrun"); dry.Skip || len(dry.Nodes) != 4 {
		t.Errorf("dryrun = %+v, want it to run on all four nodes", dry)
	}
	// The bake will have produced a golden by then, so that is what a
	// rehearsal must stream.
	if p.dryrunImg.Name != cluster.GoldenImage.Name {
		t.Errorf("dryrun image = %q, want %q", p.dryrunImg.Name, cluster.GoldenImage.Name)
	}
}

func TestPlanWithoutRehearseHasNoDryrun(t *testing.T) {
	p := newFake(t).plan(t, Options{})
	if got := strings.Join(stepIDs(p), " "); strings.Contains(got, "dryrun") {
		t.Errorf("steps = %q, want no dryrun without -rehearse", got)
	}
}

func TestPlanEstimateCountsOnlyRunningSteps(t *testing.T) {
	f := newFake(t)
	quiet := f.plan(t, Options{})
	if got, want := quiet.Estimate(), ProbeEstimate+StatusEstimate; got != want {
		t.Errorf("a no-op plan estimates %s, want %s", got, want)
	}

	f2 := newFake(t)
	f2.fingerprint = "fp-new"
	busy := f2.plan(t, Options{})
	if busy.Estimate() <= quiet.Estimate() {
		t.Errorf("a full rebuild estimates %s, no more than the no-op %s", busy.Estimate(), quiet.Estimate())
	}
}

func TestSummaryReadsAsAPlan(t *testing.T) {
	f := newFake(t)
	f.node("rasputin002").Adopted = false
	s := f.plan(t, Options{}).Summary()
	for _, want := range []string{
		"plan for cluster test",
		"RUN   adopt",
		"SKIP  prepare",
		"rasputin002",
		"estimated total",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the summary is missing %q:\n%s", want, s)
		}
	}
}
