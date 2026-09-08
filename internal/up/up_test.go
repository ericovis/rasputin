package up

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/events"
	"github.com/ericovis/rasputin/internal/prepare"
	"github.com/ericovis/rasputin/internal/server"
)

// The cluster the tests plan against: four nodes, rasputin001 the builder,
// exactly the shape of the real one.
const testYAML = `
cluster: test
image:
  source_url: https://example.invalid/raspios
  rootfs_size_gb: 4
ssh:
  key: /dev/null
  users: [berry]
provision:
  user: berry
  authorized_keys: /dev/null
  packages: [curl]
builder: rasputin001
nodes:
  - { name: rasputin001, mac: "b8:27:eb:00:00:01" }
  - { name: rasputin002, mac: "b8:27:eb:00:00:02" }
  - { name: rasputin003, mac: "b8:27:eb:00:00:03" }
  - { name: rasputin004, mac: "b8:27:eb:00:00:04" }
`

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(testYAML), "rasputin.yaml")
	if err != nil {
		t.Fatalf("parsing the test config: %v", err)
	}
	return cfg
}

// fake stands in for a real cluster, a real HTTP server and a real out/
// directory. Every hardware operation is a recorded no-op.
type fake struct {
	cfg    *config.Config
	status []cluster.Status
	prep   *prepare.Meta
	golden *cluster.GoldenMeta
	// fingerprint is what the config hashes to now; prep.Fingerprint is what
	// the last prepare recorded. Differing means the YAML changed.
	fingerprint string
	missing     map[string]bool

	// mu guards progress alone: the transfer poller reads it from its own
	// goroutine while a step is running, exactly as the real server is read.
	mu       sync.Mutex
	progress []server.Progress

	prepareErr error
	adoptErr   error
	bakeErr    error
	// flashFailures names nodes whose flash comes back with an error.
	flashFailures map[string]bool

	prepared    []prepare.Options
	adopted     []string
	bakes       int
	rehearsed   []string
	flashed     []string
	flashOpts   cluster.FlashOptions
	statusCalls int
	logf        func(format string, args ...any)
}

func newFake(t *testing.T) *fake {
	t.Helper()
	cfg := testConfig(t)
	f := &fake{
		cfg:         cfg,
		fingerprint: "fp-current",
		prep: &prepare.Meta{
			BuildID:     "build-1",
			BaseImage:   "base.img",
			Fingerprint: "fp-current",
		},
		golden:  &cluster.GoldenMeta{BuildID: "build-1", CardUsed: 4 << 30},
		missing: map[string]bool{},
	}
	for _, n := range cfg.Nodes {
		f.status = append(f.status, cluster.Status{
			Name: n.Name, MAC: n.MAC, IP: "192.168.0.1" + n.Name[len(n.Name)-1:],
			Reachable: true, Adopted: true, Provisioned: true,
			Hostname: n.Name, BuildID: "build-1",
		})
	}
	return f
}

// node returns a pointer to one probed status, for tests that want to change
// what a node reports.
func (f *fake) node(name string) *cluster.Status {
	for i := range f.status {
		if f.status[i].Name == name {
			return &f.status[i]
		}
	}
	panic("no such node in the fake: " + name)
}

// staleNodes puts every node on an older build, so a flash has real work.
func (f *fake) staleNodes(build string) {
	for i := range f.status {
		f.status[i].BuildID = build
	}
}

// setProgress and progressNow stand in for the HTTP server's counters.
func (f *fake) setProgress(p ...server.Progress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.progress = p
}

func (f *fake) progressNow() []server.Progress {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.progress
}

func (f *fake) deps() Deps {
	return Deps{
		Cfg: f.cfg,
		Status: func(context.Context, []config.Node) []cluster.Status {
			f.statusCalls++
			return f.status
		},
		Prepare: func(_ context.Context, opts prepare.Options) (*prepare.Meta, error) {
			f.prepared = append(f.prepared, opts)
			if f.prepareErr != nil {
				return nil, f.prepareErr
			}
			if opts.Log != nil {
				opts.Log("building the recovery initramfs")
			}
			f.prep = &prepare.Meta{BuildID: "build-2", BaseImage: "base.img", Fingerprint: f.fingerprint}
			return f.prep, nil
		},
		Adopt: func(_ context.Context, node config.Node, _ cluster.AdoptOptions) cluster.AdoptResult {
			f.adopted = append(f.adopted, node.Name)
			if f.logf != nil {
				f.logf("%s: installing recovery.gz (%d bytes)", node.Name, 1234)
			}
			return cluster.AdoptResult{Node: node.Name, Rebooted: true, Err: f.adoptErr}
		},
		Bake: func(context.Context) (*cluster.BakeResult, error) {
			f.bakes++
			if f.logf != nil {
				f.logf("%s: sealing", f.cfg.Builder)
			}
			if f.bakeErr != nil {
				return nil, f.bakeErr
			}
			f.golden = &cluster.GoldenMeta{BuildID: f.prep.BuildID, CardUsed: 4 << 30}
			return &cluster.BakeResult{Meta: f.golden}, nil
		},
		Dryrun: func(_ context.Context, _ cluster.Image, node config.Node) cluster.DryrunResult {
			f.rehearsed = append(f.rehearsed, node.Name)
			return cluster.DryrunResult{Node: node.Name}
		},
		Flash: func(_ context.Context, _ cluster.Image, meta *cluster.GoldenMeta, targets []config.Node, opts cluster.FlashOptions) ([]cluster.FlashResult, error) {
			f.flashOpts = opts
			var out []cluster.FlashResult
			for _, n := range targets {
				f.flashed = append(f.flashed, n.Name)
				res := cluster.FlashResult{Node: n.Name, BuildID: meta.BuildID}
				if f.flashFailures[n.Name] {
					res.Err = fmt.Errorf("%s: stuck in the recovery agent", n.Name)
				}
				out = append(out, res)
			}
			return out, nil
		},
		ReadPrepareMeta: func() (*prepare.Meta, error) {
			if f.prep == nil {
				return nil, fmt.Errorf("no prepare.json")
			}
			return f.prep, nil
		},
		ReadGoldenMeta: func() (*cluster.GoldenMeta, error) {
			if f.golden == nil {
				return nil, fmt.Errorf("no golden.json")
			}
			return f.golden, nil
		},
		FileExists:  func(path string) bool { return !f.missing[path] },
		Fingerprint: func(*config.Config, string) (string, error) { return f.fingerprint, nil },
		SetLog:      func(logf func(string, ...any)) { f.logf = logf },
		Progress:    f.progressNow,
	}
}

// plan is the planning pass with the given options, failing the test on error.
func (f *fake) plan(t *testing.T, opts Options) *Plan {
	t.Helper()
	opts.LogPath = "-" // no test writes out/sync.log
	p, err := NewPlan(context.Background(), f.deps(), opts)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	return p
}

// stepByID fails the test when the step is absent. It goes through the
// accessor the package exposes, so that is the one the tests exercise.
func stepByID(t *testing.T, p *Plan, id string) Step {
	t.Helper()
	if s := p.Step(events.StepID(id)); s != nil {
		return *s
	}
	t.Fatalf("no %s step in the plan; got %s", id, strings.Join(stepIDs(p), " "))
	return Step{}
}

func stepIDs(p *Plan) []string {
	out := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		out[i] = string(s.ID)
	}
	return out
}
