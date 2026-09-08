package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/prepare"
	"github.com/ericovis/rasputin/internal/up"
)

// The cluster `sync` is exercised against: four nodes, rasputin001 the
// builder, real MACs so the config passes validation.
const upYAML = `
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

func upConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(upYAML), "rasputin.yaml")
	if err != nil {
		t.Fatalf("parsing the test config: %v", err)
	}
	return cfg
}

// upFake stands in for the cluster, the HTTP server and out/: every
// hardware operation is a recorded no-op, so the command's own logic —
// plan, confirm, report — is what the tests see.
type upFake struct {
	cfg    *config.Config
	golden *cluster.GoldenMeta
	// nodeBuild is what every node reports running.
	nodeBuild string

	bakeErr error
	bakes   int
	flashed []string
}

func newUpFake(t *testing.T) *upFake {
	t.Helper()
	return &upFake{cfg: upConfig(t), golden: &cluster.GoldenMeta{BuildID: "build-1"},
		nodeBuild: "build-1"}
}

// deps builds Deps whose every artifact is current, so a plan over it is a
// no-op unless a test takes something away.
func (f *upFake) deps() up.Deps {
	return up.Deps{
		Cfg: f.cfg,
		Status: func(_ context.Context, targets []config.Node) []cluster.Status {
			out := make([]cluster.Status, len(targets))
			for i, n := range targets {
				out[i] = cluster.Status{Name: n.Name, MAC: n.MAC, IP: "192.168.0.10",
					Reachable: true, Adopted: true, Provisioned: true,
					Hostname: n.Name, BuildID: f.nodeBuild}
			}
			return out
		},
		Prepare: func(context.Context, prepare.Options) (*prepare.Meta, error) {
			return &prepare.Meta{BuildID: "build-1", Fingerprint: "fp"}, nil
		},
		Adopt: func(_ context.Context, node config.Node, _ cluster.AdoptOptions) cluster.AdoptResult {
			return cluster.AdoptResult{Node: node.Name}
		},
		Bake: func(context.Context) (*cluster.BakeResult, error) {
			f.bakes++
			if f.bakeErr != nil {
				return nil, f.bakeErr
			}
			// A real bake writes golden.json, which the flash step of the
			// same run reads back.
			f.golden = &cluster.GoldenMeta{BuildID: "build-1"}
			return &cluster.BakeResult{Meta: f.golden}, nil
		},
		Flash: func(_ context.Context, _ cluster.Image, meta *cluster.GoldenMeta, targets []config.Node, _ cluster.FlashOptions) ([]cluster.FlashResult, error) {
			out := make([]cluster.FlashResult, len(targets))
			for i, n := range targets {
				f.flashed = append(f.flashed, n.Name)
				out[i] = cluster.FlashResult{Node: n.Name, BuildID: meta.BuildID}
			}
			return out, nil
		},
		ReadPrepareMeta: func() (*prepare.Meta, error) {
			return &prepare.Meta{BuildID: "build-1", BaseImage: "base.img", Fingerprint: "fp"}, nil
		},
		ReadGoldenMeta: func() (*cluster.GoldenMeta, error) {
			if f.golden == nil {
				return nil, errors.New("no golden image has been baked")
			}
			return f.golden, nil
		},
		FileExists:  func(string) bool { return true },
		Fingerprint: func(*config.Config, string) (string, error) { return "fp", nil },
	}
}

// upOpts is the option set every test starts from: the run log is disabled
// so tests never write into the repository's out/.
func upOpts() up.Options { return up.Options{LogPath: "-"} }

func TestSyncFlags(t *testing.T) {
	cfg := upConfig(t)
	cases := []struct {
		name      string
		args      []string
		want      up.Options
		wantPlain bool
		wantErr   string
	}{
		{
			name: "no flags is the idempotent default",
			want: up.Options{LogPath: up.DefaultLogPath},
		},
		{
			name: "-force means all three",
			args: []string{"-force"},
			want: up.Options{ForcePrepare: true, ForceBake: true, ForceFlash: true, LogPath: up.DefaultLogPath},
		},
		{
			name: "one force flag forces only its own step",
			args: []string{"-force-bake"},
			want: up.Options{ForceBake: true, LogPath: up.DefaultLogPath},
		},
		{
			name:      "the rest of the switches",
			args:      []string{"-rehearse", "-yes", "-plain", "-log", "-"},
			want:      up.Options{Rehearse: true, Yes: true, LogPath: "-"},
			wantPlain: true,
		},
		{
			name:    "a node argument is refused, sync acts on the whole cluster",
			args:    []string{"rasputin002"},
			wantErr: "takes no arguments",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, flags, err := parseSyncFlags(cfg, testOutput(&bytes.Buffer{}), tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseSyncFlags(%v) error = %v, want it to mention %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSyncFlags(%v): %v", tc.args, err)
			}
			if opts != tc.want {
				t.Errorf("options = %+v, want %+v", opts, tc.want)
			}
			if flags.Plain != tc.wantPlain {
				t.Errorf("plain = %v, want %v", flags.Plain, tc.wantPlain)
			}
		})
	}
}

// TestSyncOnACurrentCluster is the case the command exists for: everything is
// already what the config says, so nothing is confirmed and nothing is
// touched.
func TestSyncOnACurrentCluster(t *testing.T) {
	f := newUpFake(t)
	var out bytes.Buffer
	ui := syncUI{Out: testOutput(&out), Plain: true, Confirm: func() (bool, error) {
		t.Error("a run that wipes nothing asked for confirmation")
		return false, nil
	}}

	if err := executeSync(context.Background(), f.deps(), upOpts(), ui); err != nil {
		t.Fatalf("up on a current cluster: %v", err)
	}
	if f.bakes != 0 || len(f.flashed) != 0 {
		t.Errorf("a current cluster was baked %d time(s) and flashed %v", f.bakes, f.flashed)
	}
	for _, want := range []string{
		"plan for cluster test",    // the plan, before anything runs
		"SKIP", "STEP      RESULT", // the result table
		"NODE", "rasputin004", // the status table
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
}

// TestSyncAsksBeforeWiping guards the one thing that must never be automatic.
func TestSyncAsksBeforeWiping(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil // nothing baked yet, so the builder gets wiped

	var out bytes.Buffer
	asked := false
	ui := syncUI{Out: testOutput(&out), Plain: true, Confirm: func() (bool, error) { asked = true; return false, nil }}

	err := executeSync(context.Background(), f.deps(), upOpts(), ui)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("declining the prompt returned %v, want a cancelled error", err)
	}
	if !asked {
		t.Error("a plan that wipes the builder ran without asking")
	}
	if f.bakes != 0 {
		t.Errorf("the bake ran %d time(s) after the operator said no", f.bakes)
	}
	if !strings.Contains(out.String(), "WILL WIPE") {
		t.Errorf("the operator was asked without being shown what is wiped:\n%s", out.String())
	}
}

// TestSyncWithYesSkipsThePrompt covers the unattended path CI uses.
func TestSyncWithYesSkipsThePrompt(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil
	f.nodeBuild = "build-0" // the fleet is a build behind, so cloning is real work
	opts := upOpts()
	opts.Yes = true

	var out bytes.Buffer
	ui := syncUI{Out: testOutput(&out), Plain: true, Confirm: func() (bool, error) {
		t.Error("-yes still prompted")
		return false, nil
	}}
	if err := executeSync(context.Background(), f.deps(), opts, ui); err != nil {
		t.Fatalf("sync -yes: %v", err)
	}
	if f.bakes != 1 {
		t.Errorf("bakes = %d, want exactly one", f.bakes)
	}
	want := []string{"rasputin002", "rasputin003", "rasputin004"}
	if strings.Join(f.flashed, " ") != strings.Join(want, " ") {
		t.Errorf("flashed %v, want %v (the builder keeps the image it just baked)", f.flashed, want)
	}
}

// TestSyncWithoutATerminalDemandsYes: an unconfirmable prompt must say what to
// pass instead, not hang or assume yes.
func TestSyncWithoutATerminalDemandsYes(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil

	var out bytes.Buffer
	err := executeSync(context.Background(), f.deps(), upOpts(), syncUI{Out: testOutput(&out), Plain: true})
	if err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Fatalf("error = %v, want it to tell the operator to pass -yes", err)
	}
}

// TestSyncReportsAFailedStep: the run stops, the table says which step broke,
// and the command exits with an error.
func TestSyncReportsAFailedStep(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil
	f.bakeErr = errors.New("the builder never came back")
	opts := upOpts()
	opts.Yes = true

	var out bytes.Buffer
	err := executeSync(context.Background(), f.deps(), opts, syncUI{Out: testOutput(&out), Plain: true})
	if err == nil || !strings.Contains(err.Error(), "never came back") {
		t.Fatalf("error = %v, want the bake failure", err)
	}
	if len(f.flashed) != 0 {
		t.Errorf("flashed %v after the bake failed; a failed step must stop the run", f.flashed)
	}
	for _, want := range []string{"bake      FAIL", "never came back", "flash     SKIP"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("result table is missing %q:\n%s", want, out.String())
		}
	}
}

// TestSyncAbortIsNotAFailure: ctrl-c cancels the context; the operator gets
// told the cluster is safe rather than shown a crash.
func TestSyncAbortIsNotAFailure(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil
	f.bakeErr = context.Canceled
	opts := upOpts()
	opts.Yes = true

	var out bytes.Buffer
	err := executeSync(context.Background(), f.deps(), opts, syncUI{Out: testOutput(&out), Plain: true})
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("error = %v, want an abort, not a step failure", err)
	}
	if !strings.Contains(out.String(), "recovery agent") {
		t.Errorf("the abort message does not say the nodes are safe:\n%s", out.String())
	}
}

// TestSyncAbortsPlanningOnAnUnreachableNode: planning contacts the nodes, and
// a fleet with a hole in it must not be half-flashed.
func TestSyncAbortsPlanningOnAnUnreachableNode(t *testing.T) {
	f := newUpFake(t)
	deps := f.deps()
	status := deps.Status
	deps.Status = func(ctx context.Context, targets []config.Node) []cluster.Status {
		out := status(ctx, targets)
		out[2].Reachable, out[2].Err = false, errors.New("no route to host")
		return out
	}

	var out bytes.Buffer
	err := executeSync(context.Background(), deps, upOpts(), syncUI{Out: testOutput(&out), Plain: true})
	if err == nil || !strings.Contains(err.Error(), "rasputin003") {
		t.Fatalf("error = %v, want it to name the node that does not answer", err)
	}
	if f.bakes != 0 || len(f.flashed) != 0 {
		t.Error("planning touched the cluster although a node was down")
	}
}

// TestTuiStepsMirrorThePlan guards the hand-off between the two Step types,
// which are copies of each other so tui cannot import up.
func TestTuiStepsMirrorThePlan(t *testing.T) {
	f := newUpFake(t)
	plan, err := up.NewPlan(context.Background(), f.deps(), upOpts())
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	steps := tuiSteps(plan.Steps)
	if len(steps) != len(plan.Steps) {
		t.Fatalf("tuiSteps returned %d steps, want %d", len(steps), len(plan.Steps))
	}
	for i, s := range plan.Steps {
		got := steps[i]
		if got.ID != s.ID || got.Title != s.Title || got.Skip != s.Skip ||
			got.Reason != s.Reason || got.Estimate != s.Estimate ||
			strings.Join(got.Nodes, " ") != strings.Join(s.Nodes, " ") {
			t.Errorf("step %d = %+v, want a copy of %+v", i, got, s)
		}
	}
}

// TestSyncDoesNotPromiseWipesItThenSkips: a re-bake off an unchanged prepare
// mints the same build id, so nodes already running it are neither wiped nor
// announced as about to be.
func TestSyncDoesNotPromiseWipesItThenSkips(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil // golden.json is gone, so the bake runs
	opts := upOpts()
	opts.Yes = true

	var out bytes.Buffer
	if err := executeSync(context.Background(), f.deps(), opts, syncUI{Out: testOutput(&out), Plain: true}); err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(f.flashed) != 0 {
		t.Errorf("flashed %v; every node already runs the build the bake produces", f.flashed)
	}
	wipes := firstMatch(out.String(), "WILL WIPE")
	for _, node := range []string{"rasputin002", "rasputin003", "rasputin004"} {
		if strings.Contains(wipes, node) {
			t.Errorf("%q promises to wipe %s, which the run then skips", wipes, node)
		}
	}
	if !strings.Contains(wipes, "rasputin001") {
		t.Errorf("%q does not name the builder, which the bake really does wipe", wipes)
	}
}

// TestSyncDoesNotPrintThePreRunProbeAsTheFinalState: after a failed run the
// health table would be the planning probe, which describes a fleet that no
// longer exists — the builder in it was wiped mid-bake.
func TestSyncDoesNotPrintThePreRunProbeAsTheFinalState(t *testing.T) {
	f := newUpFake(t)
	f.golden = nil
	f.bakeErr = errors.New("the builder never came back")
	opts := upOpts()
	opts.Yes = true

	var out bytes.Buffer
	if err := executeSync(context.Background(), f.deps(), opts, syncUI{Out: testOutput(&out), Plain: true}); err == nil {
		t.Fatal("a failed bake returned no error")
	}
	if strings.Contains(out.String(), "PROV") {
		t.Errorf("the health table was printed although status never ran:\n%s", out.String())
	}
}

// firstMatch returns the first line of s containing want, or "".
func firstMatch(s, want string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}
