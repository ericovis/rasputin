package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericovis/rasputin/internal/state"
)

// pinnedState points the command at a temporary cache — never the
// repository's out/ — with two nodes pinned and located.
func pinnedState(t *testing.T) *state.Store {
	t.Helper()
	old := statePath
	statePath = filepath.Join(t.TempDir(), "state.json")
	t.Cleanup(func() { statePath = old })

	st, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []struct{ name, ip, key string }{
		{"rasputin002", "192.168.0.12", "ssh-ed25519 AAAAtwo"},
		{"rasputin003", "192.168.0.13", "ssh-ed25519 AAAAthree"},
	} {
		if err := st.Update(n.name, func(rec *state.Node) { rec.IP, rec.HostKey = n.ip, n.key }); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// reload reads the cache back from disk, which is what the next run sees.
func reload(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestForgetDropsOnlyTheNamedPin is the command's whole job: one node's key
// goes, everything else — including the cached address that makes the next
// connection fast — stays.
func TestForgetDropsOnlyTheNamedPin(t *testing.T) {
	pinnedState(t)
	var out bytes.Buffer
	if err := runForget(upConfig(t), testOutput(&out), []string{"rasputin002"}); err != nil {
		t.Fatalf("forget rasputin002: %v", err)
	}
	if !strings.Contains(out.String(), "rasputin002: forgot pinned host key") {
		t.Errorf("output = %q", out.String())
	}

	st := reload(t)
	if got := st.HostKey("rasputin002"); got != "" {
		t.Errorf("host key = %q after forget, want it dropped", got)
	}
	n, _ := st.Get("rasputin002")
	if n.IP != "192.168.0.12" {
		t.Errorf("forget lost the cached address: %+v", n)
	}
	if st.HostKey("rasputin003") == "" {
		t.Error("forget dropped a pin it was not asked about")
	}
}

func TestForgetAllAndAnUnpinnedNode(t *testing.T) {
	pinnedState(t)
	var out bytes.Buffer
	if err := runForget(upConfig(t), testOutput(&out), []string{"all"}); err != nil {
		t.Fatalf("forget all: %v", err)
	}
	st := reload(t)
	for _, name := range []string{"rasputin002", "rasputin003"} {
		if got := st.HostKey(name); got != "" {
			t.Errorf("%s still pinned as %q", name, got)
		}
	}
	// rasputin001 and 004 were never pinned: saying so is not a failure.
	if !strings.Contains(out.String(), "rasputin001: no pinned host key") {
		t.Errorf("output = %q, want it to report the nodes with nothing pinned", out.String())
	}
	if err := runForget(upConfig(t), testOutput(&bytes.Buffer{}), []string{"rasputin002"}); err != nil {
		t.Errorf("forgetting an unpinned node failed: %v", err)
	}
}

func TestForgetRejectsBadArguments(t *testing.T) {
	pinnedState(t)
	cases := map[string][]string{
		"no node given": nil,
		"is not a node": {"rasputin009"},
	}
	for want, args := range cases {
		var out bytes.Buffer
		err := runForget(upConfig(t), testOutput(&out), args)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("forget %v: err = %v, want it to mention %q", args, err, want)
		}
		if out.Len() != 0 {
			t.Errorf("forget %v wrote %q before refusing", args, out.String())
		}
	}
	// The refusal has to say what the valid names are.
	err := runForget(upConfig(t), testOutput(&bytes.Buffer{}), []string{"rasputin009"})
	if err == nil || !strings.Contains(err.Error(), "rasputin001") {
		t.Errorf("err = %v, want it to list the configured nodes", err)
	}
	if st := reload(t); st.HostKey("rasputin002") == "" {
		t.Error("a refused forget still edited the cache")
	}
}

func TestForgetJSON(t *testing.T) {
	pinnedState(t)
	var buf bytes.Buffer
	if err := runForget(upConfig(t), jsonOutput(&buf), []string{"rasputin002", "rasputin001"}); err != nil {
		t.Fatal(err)
	}
	got := result(t, &buf)
	if got["ok"] != true || got["command"] != "forget" || got["forgotten"] != float64(1) {
		t.Errorf("result = %v", got)
	}
	nodes, _ := got["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %v, want both arguments, in config order", got["nodes"])
	}
	first, _ := nodes[0].(map[string]any)
	second, _ := nodes[1].(map[string]any)
	if first["node"] != "rasputin001" || first["forgotten"] != false {
		t.Errorf("nodes[0] = %v, want rasputin001 with nothing pinned", first)
	}
	if second["node"] != "rasputin002" || second["forgotten"] != true {
		t.Errorf("nodes[1] = %v, want rasputin002 forgotten", second)
	}
}
