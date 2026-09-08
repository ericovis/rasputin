// Package up is the orchestrator behind `rasputin sync`: one idempotent
// command that puts the whole cluster into the state rasputin.yaml
// describes, skipping every step whose output is already current.
//
// It is split in two on purpose. NewPlan is a read-only pass that decides
// what would happen and can be shown to the operator before anything is
// touched; Run executes that plan and reports progress as events. Nothing
// here knows about terminals: the only output channel is an events.Sink.
package up

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/prepare"
	"github.com/ericovis/rasputin/internal/server"
)

// DefaultLogPath is where every event of a run is appended in plain form,
// so the TUI can show a short tail and the whole story still survives.
const DefaultLogPath = "out/sync.log"

// Measured on four Pi 3 B over 100 Mbit ethernet (2026-08-29). They are
// estimates for the progress bar and the plan summary, never timeouts.
const (
	ProbeEstimate       = 2 * time.Second
	PrepareEstimate     = 30 * time.Second
	PrepareColdEstimate = 4 * time.Minute // the first run downloads ~500 MB
	AdoptEstimate       = time.Minute     // per node, sequential
	BakeEstimate        = 16 * time.Minute
	DryrunEstimate      = 2 * time.Minute // per node, sequential
	FlashOneEstimate    = 6 * time.Minute
	FlashManyEstimate   = 7 * time.Minute // parallel; the wire is the limit
	StatusEstimate      = 2 * time.Second
)

// Options are the operator's overrides.
type Options struct {
	// ForcePrepare, ForceBake and ForceFlash each turn one skip decision off.
	ForcePrepare bool
	ForceBake    bool
	ForceFlash   bool
	// Rehearse inserts a dryrun before the flash step.
	Rehearse bool
	// Yes is the caller's "do not ask"; up itself never prompts, it only
	// reports NeedsConfirmation, but the flag travels with the plan so the
	// caller can record what it was run with.
	Yes bool
	// LogPath is the append-only plain log of the run. Empty means
	// DefaultLogPath; "-" disables it.
	LogPath string
}

// Deps is everything up needs from the outside world, as function fields so
// the whole orchestrator can be tested without a Pi, a server or a disk.
// NewDeps builds the real thing from a Cluster and its HTTP server.
type Deps struct {
	// Cfg is the cluster configuration; required.
	Cfg *config.Config
	// Targets is what to act on; empty means every configured node.
	Targets []config.Node

	// Status probes nodes read-only. Required.
	Status func(ctx context.Context, targets []config.Node) []cluster.Status
	// Prepare builds the artifacts. Required.
	Prepare func(ctx context.Context, opts prepare.Options) (*prepare.Meta, error)
	// Adopt installs the recovery mechanism on one node. Required.
	Adopt func(ctx context.Context, node config.Node, opts cluster.AdoptOptions) cluster.AdoptResult
	// Bake produces the golden image on the builder. Required.
	Bake func(ctx context.Context) (*cluster.BakeResult, error)
	// Dryrun rehearses a flash on one node. Required with Options.Rehearse.
	Dryrun func(ctx context.Context, img cluster.Image, node config.Node) cluster.DryrunResult
	// Flash clones the golden image onto nodes, in parallel. Required.
	Flash func(ctx context.Context, img cluster.Image, meta *cluster.GoldenMeta, targets []config.Node, opts cluster.FlashOptions) ([]cluster.FlashResult, error)

	// ReadPrepareMeta and ReadGoldenMeta report what the last prepare and
	// the last bake produced. Both return an error when nothing is recorded.
	ReadPrepareMeta func() (*prepare.Meta, error)
	ReadGoldenMeta  func() (*cluster.GoldenMeta, error)
	// FileExists reports whether an artifact is on disk and non-empty.
	FileExists func(path string) bool
	// Fingerprint is prepare's idempotency key.
	Fingerprint func(cfg *config.Config, baseImage string) (string, error)

	// SetLog points the cluster's logger at the run's event sink. It is
	// called between steps, never during one.
	SetLog func(logf func(format string, args ...any))
	// Progress reports the HTTP server's per-client download progress, and
	// StagePath names the growing file of a capture. Both are optional:
	// without them a run simply emits no Transfer events.
	Progress  func() []server.Progress
	StagePath func(captureID string) string
}

// NewDeps wires a real cluster and its server into Deps.
func NewDeps(c *cluster.Cluster, srv *server.Server) Deps {
	return Deps{
		Cfg:    c.Cfg,
		Status: func(ctx context.Context, targets []config.Node) []cluster.Status { return c.Status(ctx, targets) },
		Prepare: func(ctx context.Context, opts prepare.Options) (*prepare.Meta, error) {
			return prepare.Run(ctx, c.Cfg, opts)
		},
		Adopt: func(ctx context.Context, node config.Node, opts cluster.AdoptOptions) cluster.AdoptResult {
			return c.Adopt(ctx, node, opts)
		},
		Bake: func(ctx context.Context) (*cluster.BakeResult, error) { return c.Bake(ctx, srv) },
		Dryrun: func(ctx context.Context, img cluster.Image, node config.Node) cluster.DryrunResult {
			// Forget the bake's transfers: without this the byte counters of
			// the builder's own download are still in the server and the next
			// step reports them as its own progress.
			srv.ResetProgress()
			if err := srv.Register(img.Name, img.Path); err != nil {
				return cluster.DryrunResult{Node: node.Name, Err: err}
			}
			return c.Dryrun(ctx, srv, img, node)
		},
		Flash: func(ctx context.Context, img cluster.Image, meta *cluster.GoldenMeta, targets []config.Node, opts cluster.FlashOptions) ([]cluster.FlashResult, error) {
			srv.ResetProgress()
			if err := srv.Register(img.Name, img.Path); err != nil {
				return nil, err
			}
			return c.Flash(ctx, srv, img, meta, targets, opts), nil
		},
		ReadPrepareMeta: prepare.ReadMeta,
		ReadGoldenMeta:  cluster.ReadGoldenMeta,
		FileExists:      FileExists,
		Fingerprint:     prepare.Fingerprint,
		// Both loggers, not just c.Log: the resolver keeps its own copy of
		// the function it was built with, and its lines (address changes,
		// MAC verification) would otherwise still go to stdout and tear a
		// TUI apart.
		SetLog: func(logf func(string, ...any)) {
			c.Log = logf
			if c.Resolver != nil {
				c.Resolver.Log = logf
			}
		},
		Progress:  srv.Progress,
		StagePath: srv.StagePath,
	}
}

// FileExists reports whether path is a non-empty file, which is what "the
// artifact is there" means for every output up looks at.
func FileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

// check validates the wiring before anything is contacted: a nil function
// field is a programming error, and finding it here beats a panic halfway
// through a bake.
func (d Deps) check(opts Options) error {
	if d.Cfg == nil {
		return fmt.Errorf("up: Deps.Cfg is required")
	}
	required := []struct {
		name string
		nil  bool
	}{
		{"Status", d.Status == nil},
		{"Prepare", d.Prepare == nil},
		{"Adopt", d.Adopt == nil},
		{"Bake", d.Bake == nil},
		{"Flash", d.Flash == nil},
		{"ReadPrepareMeta", d.ReadPrepareMeta == nil},
		{"ReadGoldenMeta", d.ReadGoldenMeta == nil},
		{"FileExists", d.FileExists == nil},
		{"Fingerprint", d.Fingerprint == nil},
		{"Dryrun", opts.Rehearse && d.Dryrun == nil},
	}
	for _, r := range required {
		if r.nil {
			return fmt.Errorf("up: Deps.%s is required", r.name)
		}
	}
	return nil
}

// targets is the node set to act on.
func (d Deps) targets() []config.Node {
	if len(d.Targets) > 0 {
		return d.Targets
	}
	return d.Cfg.Nodes
}

// setLog points the cluster logger at logf when a real cluster is behind
// Deps, and does nothing otherwise.
func (d Deps) setLog(logf func(format string, args ...any)) {
	if d.SetLog != nil {
		d.SetLog(logf)
	}
}

func names(nodes []config.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name
	}
	return out
}
