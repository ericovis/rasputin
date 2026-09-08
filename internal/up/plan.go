package up

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/events"
	"github.com/ericovis/rasputin/internal/prepare"
	"github.com/ericovis/rasputin/internal/vanilla"
)

// Step is one stage of a run, as decided before anything is touched.
type Step struct {
	ID    events.StepID
	Title string
	// Skip says the step's output is already current; Reason says why, in
	// words meant for the operator.
	Skip   bool
	Reason string
	// Nodes are the nodes the step will act on.
	Nodes []string
	// Estimate is how long the step usually takes, for the progress bar.
	Estimate time.Duration
	// Wipes are the nodes this step destroys. Anything listed here is why
	// `sync` asks before it starts.
	Wipes []string
}

// Plan is the outcome of a read-only planning pass. It also carries the
// decisions Run needs, so the run does exactly what was shown.
type Plan struct {
	// Cluster is the configured cluster name, for the summary header.
	Cluster string
	// Steps are in execution order.
	Steps []Step
	// Status is the probe every decision was made from.
	Status []cluster.Status

	opts    Options
	cfg     *config.Config
	targets []config.Node
	// probe is the probe result per node MAC. Never trust a hostname, and
	// never trust a position either: the probe is paired back to the node it
	// describes by the identity that matters.
	probe map[string]cluster.Status

	adoptTargets []config.Node
	flashTargets []config.Node
	flashSkipped []string
	// prepareMeta and goldenMeta are what was on disk at planning time;
	// either may be nil.
	prepareMeta *prepare.Meta
	goldenMeta  *cluster.GoldenMeta
	// expectedBuild is the build id nodes should end up on, empty when a
	// bake is about to produce a new one.
	expectedBuild string
	// dryrunImg is what a rehearsal would stream.
	dryrunImg cluster.Image
}

// NewPlan probes the cluster and decides what a run would do.
//
// The probe is the plan's foundation, so an unreachable node aborts here,
// before any node has been written to: a half-planned fleet is worse than
// no run at all.
func NewPlan(ctx context.Context, deps Deps, opts Options) (*Plan, error) {
	if err := deps.check(opts); err != nil {
		return nil, err
	}
	cfg := deps.Cfg
	targets := deps.targets()

	status := deps.Status(ctx, targets)
	var down []string
	for _, s := range status {
		if !s.Reachable {
			down = append(down, fmt.Sprintf("%s (%v)", s.Name, s.Err))
		}
	}
	if len(down) > 0 {
		return nil, fmt.Errorf("%d of %d node(s) do not answer: %s\n"+
			"nothing has been touched. Power them up, or drop them from %s, then run `rasputin sync` again",
			len(down), len(status), strings.Join(down, "; "), cfg.Path)
	}

	p := &Plan{
		Cluster: cfg.Cluster,
		Status:  status,
		opts:    opts,
		cfg:     cfg,
		targets: targets,
		probe:   make(map[string]cluster.Status, len(status)),
	}
	for _, s := range status {
		p.probe[strings.ToLower(s.MAC)] = s
	}
	for _, n := range targets {
		if _, ok := p.probe[strings.ToLower(n.MAC)]; !ok {
			return nil, fmt.Errorf("the probe returned no result for %s (%s); "+
				"nothing has been touched", n.Name, n.MAC)
		}
	}
	if m, err := deps.ReadPrepareMeta(); err == nil {
		p.prepareMeta = m
	}
	if m, err := deps.ReadGoldenMeta(); err == nil {
		p.goldenMeta = m
	}

	p.Steps = append(p.Steps, Step{
		ID:       events.StepProbe,
		Title:    fmt.Sprintf("%s reachable", plural(len(status), "node")),
		Nodes:    names(targets),
		Estimate: ProbeEstimate,
	})

	prepareStep, err := p.planPrepare(deps)
	if err != nil {
		return nil, err
	}
	p.Steps = append(p.Steps, prepareStep, p.planAdopt())

	bakeStep := p.planBake(deps, !prepareStep.Skip)
	p.Steps = append(p.Steps, bakeStep)

	// The build every node should end up on. A bake writes the prepare's own
	// build id into the golden (cluster.Bake), so a re-bake off an unchanged
	// prepare produces the id that is already on disk — and the nodes already
	// running it must not be listed as about to be wiped. Only a prepare
	// mints a new id, and that is the one case this stays empty for.
	switch {
	case bakeStep.Skip && p.goldenMeta != nil:
		p.expectedBuild = p.goldenMeta.BuildID
	case !bakeStep.Skip && prepareStep.Skip && p.prepareMeta != nil:
		p.expectedBuild = p.prepareMeta.BuildID
	}

	if opts.Rehearse {
		p.Steps = append(p.Steps, p.planDryrun(deps, !bakeStep.Skip))
	}
	p.Steps = append(p.Steps, p.planFlash(!bakeStep.Skip), Step{
		ID:       events.StepStatus,
		Title:    "read the health table back",
		Nodes:    names(targets),
		Estimate: StatusEstimate,
	})
	return p, nil
}

// planPrepare decides whether the artifacts in out/ still match the config.
func (p *Plan) planPrepare(deps Deps) (Step, error) {
	step := Step{ID: events.StepPrepare, Estimate: PrepareEstimate,
		Title: "build recovery.gz and the customised stock image"}
	// The stock image is ~500 MB and only downloaded once; say so, because
	// an unexplained four-minute first step looks like a hang.
	if !deps.FileExists(vanilla.DefaultMetaPath) {
		step.Title += " (first run: downloads ~500 MB)"
		step.Estimate = PrepareColdEstimate
	}

	switch {
	case p.opts.ForcePrepare:
		step.Reason = "-force-prepare was given"
		return step, nil
	case p.prepareMeta == nil:
		step.Reason = "nothing has been prepared yet"
		return step, nil
	case !deps.FileExists(prepare.RecoveryPath):
		step.Reason = prepare.RecoveryPath + " is missing"
		return step, nil
	case !deps.FileExists(prepare.ImageZstPath):
		step.Reason = prepare.ImageZstPath + " is missing"
		return step, nil
	}

	fp, err := deps.Fingerprint(p.cfg, p.prepareMeta.BaseImage)
	if err != nil {
		return Step{}, fmt.Errorf("cannot fingerprint the configuration: %w", err)
	}
	switch {
	case p.prepareMeta.Fingerprint == "":
		step.Reason = "the last prepare recorded no fingerprint"
	case p.prepareMeta.Fingerprint != fp:
		step.Reason = fmt.Sprintf("%s changed since the last prepare", p.cfg.Path)
	default:
		step.Skip = true
		step.Reason = fmt.Sprintf("the prepared image matches %s (build %s)", p.cfg.Path, p.prepareMeta.BuildID)
	}
	return step, nil
}

// planAdopt lists the nodes that still boot without the recovery agent. A
// node already running a rasputin build is adopted by construction.
func (p *Plan) planAdopt() Step {
	step := Step{ID: events.StepAdopt, Title: "install the recovery agent"}
	for _, node := range p.targets {
		if !p.probeOf(node).Adopted {
			p.adoptTargets = append(p.adoptTargets, node)
		}
	}
	if len(p.adoptTargets) == 0 {
		step.Skip = true
		step.Reason = "every node already boots through the recovery agent"
		return step
	}
	step.Nodes = names(p.adoptTargets)
	step.Estimate = time.Duration(len(p.adoptTargets)) * AdoptEstimate
	step.Reason = plural(len(p.adoptTargets), "node") + " not adopted yet"
	return step
}

// planBake decides whether the golden image is still the one this prepare
// would produce.
func (p *Plan) planBake(deps Deps, prepareWillRun bool) Step {
	step := Step{
		ID:       events.StepBake,
		Title:    "bake the golden image on " + p.cfg.Builder,
		Nodes:    []string{p.cfg.Builder},
		Wipes:    []string{p.cfg.Builder},
		Estimate: BakeEstimate,
	}
	switch {
	case p.opts.ForceBake:
		step.Reason = "-force-bake was given"
	case prepareWillRun:
		step.Reason = "the prepared image is being rebuilt"
	case p.goldenMeta == nil:
		step.Reason = "no golden image has been baked yet"
	case !deps.FileExists(cluster.GoldenImage.Path):
		step.Reason = cluster.GoldenImage.Path + " is missing"
	case p.prepareMeta != nil && p.goldenMeta.BuildID != p.prepareMeta.BuildID:
		step.Reason = fmt.Sprintf("the golden image is build %s, the prepared image is %s",
			p.goldenMeta.BuildID, p.prepareMeta.BuildID)
	default:
		step.Skip = true
		step.Wipes = nil
		step.Reason = "the golden image matches the prepared image (build " + p.goldenMeta.BuildID + ")"
	}
	return step
}

// planDryrun rehearses the whole download-and-decode pipeline on every node
// without opening a card for writing.
func (p *Plan) planDryrun(deps Deps, bakeWillRun bool) Step {
	p.dryrunImg = p.dryrunImage(deps, bakeWillRun)
	return Step{
		ID:       events.StepDryrun,
		Title:    "rehearse a flash from " + p.dryrunImg.Name,
		Nodes:    names(p.targets),
		Estimate: time.Duration(len(p.targets)) * DryrunEstimate,
		Reason:   "-rehearse was given",
	}
}

// dryrunImage mirrors cluster.BestImage: the golden image if there will be
// one by the time the dryrun runs, the prepared stock image otherwise.
func (p *Plan) dryrunImage(deps Deps, bakeWillRun bool) cluster.Image {
	if bakeWillRun || deps.FileExists(cluster.GoldenImage.Path) {
		return cluster.GoldenImage
	}
	return cluster.VanillaImage
}

// planFlash decides, per node, whether it already runs the build it should.
func (p *Plan) planFlash(bakeWillRun bool) Step {
	step := Step{ID: events.StepFlash, Title: "clone the golden image", Estimate: FlashManyEstimate}
	for _, node := range p.targets {
		s := p.probeOf(node)
		switch {
		case p.opts.ForceFlash:
		case bakeWillRun && node.Name == p.cfg.Builder:
			// The bake leaves the builder running the image it just made.
			p.flashSkipped = append(p.flashSkipped, node.Name)
			continue
		case p.expectedBuild != "" && s.BuildID == p.expectedBuild:
			p.flashSkipped = append(p.flashSkipped, node.Name)
			continue
		}
		p.flashTargets = append(p.flashTargets, node)
	}
	if len(p.flashTargets) == 0 {
		step.Skip = true
		step.Reason = "every node already runs golden build " + p.expectedBuild
		if p.expectedBuild == "" {
			step.Reason = "there is nothing left to clone"
		}
		return step
	}
	step.Nodes = names(p.flashTargets)
	step.Wipes = step.Nodes
	if len(p.flashTargets) == 1 {
		step.Estimate = FlashOneEstimate
	}
	step.Reason = plural(len(p.flashTargets), "node") + " to clone"
	if len(p.flashSkipped) > 0 {
		step.Reason += fmt.Sprintf(" (%s already current)", strings.Join(p.flashSkipped, " "))
	}
	return step
}

// probeOf is a node's probe result, paired by MAC. NewPlan has already
// refused a plan whose probe is missing a node, so the zero value here would
// be a programming error rather than a state to handle.
func (p *Plan) probeOf(node config.Node) cluster.Status {
	return p.probe[strings.ToLower(node.MAC)]
}

// Step returns the planned step with that id, or nil.
func (p *Plan) Step(id events.StepID) *Step {
	for i := range p.Steps {
		if p.Steps[i].ID == id {
			return &p.Steps[i]
		}
	}
	return nil
}

// Wipes lists every node the plan destroys, each with the step that does it,
// in execution order.
func (p *Plan) Wipes() []string {
	var out []string
	for _, s := range p.Steps {
		if s.Skip {
			continue
		}
		for _, n := range s.Wipes {
			out = append(out, fmt.Sprintf("%s (%s)", n, s.ID))
		}
	}
	return out
}

// NeedsConfirmation is true when the run would destroy a node's card.
func (p *Plan) NeedsConfirmation() bool { return len(p.Wipes()) > 0 }

// Estimate is how long the whole run should take.
func (p *Plan) Estimate() time.Duration {
	var total time.Duration
	for _, s := range p.Steps {
		if !s.Skip {
			total += s.Estimate
		}
	}
	return total
}

// Summary is the plan as the operator sees it before confirming.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan for cluster %s — %s\n\n", p.Cluster, plural(len(p.Status), "node"))
	for _, s := range p.Steps {
		verb, note := "RUN ", s.Title
		if s.Skip {
			verb, note = "SKIP", "skipped — "+s.Reason
		}
		fmt.Fprintf(&b, "  %s  %-8s %s\n", verb, s.ID, note)
		if !s.Skip && len(s.Nodes) > 0 && s.ID != events.StepProbe {
			fmt.Fprintf(&b, "                 %s\n", strings.Join(s.Nodes, " "))
		}
	}
	if w := p.Wipes(); len(w) > 0 {
		fmt.Fprintf(&b, "\nWILL WIPE: %s\n", strings.Join(w, ", "))
	}
	fmt.Fprintf(&b, "\nestimated total: ~%s\n", round(p.Estimate()))
	return b.String()
}

// plural renders a count with its noun, so messages read like prose.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// round trims an estimate to something a human reads at a glance.
func round(d time.Duration) time.Duration {
	if d >= time.Minute {
		return d.Round(time.Minute)
	}
	return d.Round(time.Second)
}
