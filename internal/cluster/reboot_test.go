package cluster

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/nodes"
	"github.com/ericovis/rasputin/internal/sshx"
	"github.com/ericovis/rasputin/internal/state"
)

const rebootYAML = `
cluster: rasputin
image:
  source_url: https://example.invalid/x.img.xz
ssh:
  key: /dev/null
  users: [berry]
provision:
  user: berry
  authorized_keys: /dev/null
builder: rasputin001
nodes:
  - { name: rasputin001, mac: "b8:27:eb:01:02:03" }
`

// rebootConn is a node that answers a handful of commands.
type rebootConn struct{ closed bool }

func (c *rebootConn) Run(cmd string) (sshx.Result, error) {
	switch cmd {
	case "cat " + nodes.MACPath:
		return sshx.Result{Stdout: "b8:27:eb:01:02:03\n"}, nil
	case "sudo -n systemctl --no-block reboot":
		return sshx.Result{}, nil
	}
	return sshx.Result{}, nil
}
func (c *rebootConn) Sudo(cmd string) (sshx.Result, error) { return c.Run("sudo -n " + cmd) }
func (c *rebootConn) Output(cmd string) (string, error) {
	r, err := c.Run(cmd)
	return trim(r.Stdout), err
}
func (c *rebootConn) Push(string, []byte, string) error { return nil }
func (c *rebootConn) Fetch(string) ([]byte, error)      { return nil, nil }
func (c *rebootConn) User() string                      { return "berry" }
func (c *rebootConn) Host() string                      { return "192.168.0.74" }
func (c *rebootConn) Close() error                      { c.closed = true; return nil }

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

// reflashDialer models a node being reflashed: it answers with the old host
// key, then goes away, then comes back presenting a NEW key. Every
// successful handshake pins the presented key when nothing is pinned yet,
// exactly as the real host-key callback does.
type reflashDialer struct {
	mu       sync.Mutex
	store    *state.Store
	dials    int
	downFrom int // dial number at which the node stops answering
	upFrom   int // dial number at which it answers again, with newKey
	oldKey   string
	newKey   string
}

func (d *reflashDialer) Dial(_ context.Context, node, host string) (nodes.Conn, error) {
	d.mu.Lock()
	d.dials++
	n := d.dials
	d.mu.Unlock()

	if n >= d.downFrom && n < d.upFrom {
		return nil, fmt.Errorf("no route to host")
	}
	key := d.oldKey
	if n >= d.upFrom {
		key = d.newKey
	}
	// This is what sshx's HostKeyCallback does on every handshake.
	if known := d.store.HostKey(node); known == "" {
		if err := d.store.SetHostKey(node, key); err != nil {
			return nil, err
		}
	} else if known != key {
		return nil, fmt.Errorf("host key for %s changed", node)
	}
	return &rebootConn{}, nil
}

func newRebootCluster(t *testing.T, d *reflashDialer) (*Cluster, config.Node) {
	t.Helper()
	cfg, err := config.Parse([]byte(rebootYAML), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.store = st
	c := &Cluster{
		Cfg:   cfg,
		State: st,
		Log:   func(string, ...any) {},
		Resolver: &nodes.Resolver{
			Cfg: cfg, State: st, Dial: d,
			ARP:          func() (map[string]string, error) { return nil, nil },
			PollInterval: time.Millisecond,
			DialTimeout:  time.Second,
		},
	}
	return c, cfg.Nodes[0]
}

// TestRebootReplacingSystemClearsTheStaleHostKey is the regression test for
// a bug that hung a real bake: the pinned host key was cleared before the
// reboot, but waitGone's own polling connected once more before the node
// went down and re-pinned the key that was about to be destroyed. Every
// connection after the reflash was then refused as an impersonation.
func TestRebootReplacingSystemClearsTheStaleHostKey(t *testing.T) {
	d := &reflashDialer{
		downFrom: 3, upFrom: 5,
		oldKey: "ssh-ed25519 OLDKEY",
		newKey: "ssh-ed25519 NEWKEY",
	}
	c, node := newRebootCluster(t, d)

	// The pre-reboot connection pins the old key, as a real one would.
	conn, err := c.Resolver.Connect(context.Background(), node)
	if err != nil {
		t.Fatalf("initial Connect: %v", err)
	}
	if got := c.State.HostKey(node.Name); got != d.oldKey {
		t.Fatalf("pinned key = %q, want the old one", got)
	}

	back, err := c.RebootAndWait(context.Background(), node, conn,
		RebootOptions{Back: 10 * time.Second, ReplacesSystem: true})
	if err != nil {
		t.Fatalf("RebootAndWait: %v", err)
	}
	defer back.Close()

	if got := c.State.HostKey(node.Name); got != d.newKey {
		t.Errorf("pinned key = %q, want the new key %q — the stale pin was not cleared "+
			"after the node went down", got, d.newKey)
	}
}

// TestRebootPreservingSystemKeepsTheHostKey covers adoption, where the node
// keeps its identity: the pin must survive, so a genuinely changed key is
// still noticed.
func TestRebootPreservingSystemKeepsTheHostKey(t *testing.T) {
	d := &reflashDialer{
		downFrom: 3, upFrom: 5,
		oldKey: "ssh-ed25519 SAMEKEY",
		newKey: "ssh-ed25519 SAMEKEY",
	}
	c, node := newRebootCluster(t, d)

	conn, err := c.Resolver.Connect(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	back, err := c.RebootAndWait(context.Background(), node, conn, RebootOptions{Back: 10 * time.Second})
	if err != nil {
		t.Fatalf("RebootAndWait: %v", err)
	}
	defer back.Close()

	if got := c.State.HostKey(node.Name); got != "ssh-ed25519 SAMEKEY" {
		t.Errorf("pinned key = %q, want it preserved across a plain reboot", got)
	}
}

// TestRebootFailsWhenTheNodeNeverGoesDown proves a reboot that silently did
// nothing is reported rather than passing.
func TestRebootFailsWhenTheNodeNeverGoesDown(t *testing.T) {
	d := &reflashDialer{downFrom: 1 << 30, upFrom: 1 << 30, oldKey: "k", newKey: "k"}
	c, node := newRebootCluster(t, d)
	conn, err := c.Resolver.Connect(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := c.RebootAndWait(ctx, node, conn, RebootOptions{Back: time.Second}); err == nil {
		t.Fatal("RebootAndWait reported success for a node that never went down")
	}
}
