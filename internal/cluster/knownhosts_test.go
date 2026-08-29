package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/state"
)

func TestKnownHostCandidates(t *testing.T) {
	st, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.Seen("rasputin001", "192.168.0.74", "berry")
	c := &Cluster{State: st}

	got := c.knownHostCandidates(config.Node{Name: "rasputin001"}, []string{"rasputin001.local", "192.168.0.74"})
	want := []string{"rasputin001", "rasputin001.local", "192.168.0.74"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("candidates = %v, want %v (deduplicated, mDNS and last-seen address included)", got, want)
	}
}

func TestKnownHostCandidatesWithoutState(t *testing.T) {
	c := &Cluster{}
	got := c.knownHostCandidates(config.Node{Name: "n"}, nil)
	if strings.Join(got, ",") != "n,n.local" {
		t.Errorf("candidates = %v", got)
	}
}

// TestPurgeKnownHostsIsBestEffort proves a missing known_hosts, or a missing
// ssh-keygen, never turns a successful flash into a failure.
func TestPurgeKnownHostsIsBestEffort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var logged []string
	c := &Cluster{Log: func(f string, a ...any) { logged = append(logged, f) }}

	// No known_hosts at all: nothing happens, nothing is said.
	c.PurgeKnownHosts(config.Node{Name: "rasputin001"})
	if len(logged) != 0 {
		t.Errorf("logged %v for a missing known_hosts", logged)
	}

	// An existing file with no matching entry: still no failure.
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	if err := os.WriteFile(path, []byte("someotherhost ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.PurgeKnownHosts(config.Node{Name: "rasputin001"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("known_hosts was destroyed: %v", err)
	}
	if !strings.Contains(string(data), "someotherhost") {
		t.Error("an unrelated known_hosts entry was removed")
	}
}

func TestKnownHostsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := KnownHostsPath(), filepath.Join(home, ".ssh", "known_hosts"); got != want {
		t.Errorf("KnownHostsPath = %q, want %q", got, want)
	}
}
