package cluster

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/nodes"
	"github.com/ericovis/rasputin/internal/sshx"
	"github.com/ericovis/rasputin/internal/state"
)

// resetConn is a node as resetOne sees it: it answers the overlay probe, the
// flag check, and records the flag push and the reboot.
type resetConn struct {
	d    *resetDialer
	root string // what the overlay probe answers
	flag string // "present" or "gone" after the reboot
}

func (c *resetConn) Run(cmd string) (sshx.Result, error) {
	switch {
	case cmd == "cat "+nodes.MACPath:
		return sshx.Result{Stdout: "b8:27:eb:01:02:03\n"}, nil
	case cmd == overlayProbe:
		return sshx.Result{Stdout: c.root + "\n"}, nil
	case strings.Contains(cmd, FlagReset):
		return sshx.Result{Stdout: c.flag + "\n"}, nil
	case strings.Contains(cmd, "reboot"):
		c.d.record("reboot")
		return sshx.Result{}, nil
	}
	return sshx.Result{}, nil
}
func (c *resetConn) Sudo(cmd string) (sshx.Result, error) { return c.Run("sudo -n " + cmd) }
func (c *resetConn) Output(cmd string) (string, error) {
	r, err := c.Run(cmd)
	return strings.TrimRight(r.Stdout, "\n "), err
}
func (c *resetConn) Push(path string, _ []byte, _ string) error {
	c.d.record("push:" + path)
	return nil
}
func (c *resetConn) Fetch(string) ([]byte, error) { return nil, nil }
func (c *resetConn) User() string                 { return "berry" }
func (c *resetConn) Host() string                 { return "192.168.0.74" }
func (c *resetConn) Close() error                 { return nil }

// resetDialer models a node rebooting through the recovery agent: it answers,
// goes away, and comes back presenting the SAME host key — a reset replaces
// nothing, so the key in the read-only layer is still the node's.
type resetDialer struct {
	mu                    sync.Mutex
	store                 *state.Store
	dials                 int
	downFrom, upFrom      int
	rootBefore, rootAfter string
	flagAfter             string
	events                []string
}

func (d *resetDialer) record(e string) { d.mu.Lock(); d.events = append(d.events, e); d.mu.Unlock() }

func (d *resetDialer) seen(e string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, got := range d.events {
		if got == e {
			return true
		}
	}
	return false
}

func (d *resetDialer) Dial(_ context.Context, node, host string) (nodes.Conn, error) {
	d.mu.Lock()
	d.dials++
	n := d.dials
	d.mu.Unlock()
	if n >= d.downFrom && n < d.upFrom {
		return nil, fmt.Errorf("no route to host")
	}
	const key = "ssh-ed25519 PINNED"
	root, flag := d.rootBefore, "present"
	if n >= d.upFrom {
		root, flag = d.rootAfter, d.flagAfter
	}
	if known := d.store.HostKey(node); known == "" {
		if err := d.store.SetHostKey(node, key); err != nil {
			return nil, err
		}
	} else if known != key {
		return nil, fmt.Errorf("host key for %s changed", node)
	}
	return &resetConn{d: d, root: root, flag: flag}, nil
}

func newResetCluster(t *testing.T, d *resetDialer) (*Cluster, config.Node) {
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

// TestResetArmsTheFlagAndKeepsTheNodesIdentity is the whole point of a reset:
// the card is not rewritten, so the host key the CLI pinned is still the
// node's own and must survive untouched.
func TestResetArmsTheFlagAndKeepsTheNodesIdentity(t *testing.T) {
	sandboxHome(t)
	d := &resetDialer{
		downFrom: 2, upFrom: 4,
		rootBefore: "overlay", rootAfter: "overlay", flagAfter: "gone",
	}
	c, node := newResetCluster(t, d)

	res := c.resetOne(context.Background(), node)

	if !res.OK() {
		t.Fatalf("resetOne: %v", res.Err)
	}
	if !res.Overlay {
		t.Error("a reset node is not reported as running on a writable layer")
	}
	if !d.seen("push:" + FlagReset) {
		t.Error("no reset flag was armed")
	}
	if !d.seen("reboot") {
		t.Error("the node was never rebooted")
	}
	if got := c.State.HostKey("rasputin001"); got != "ssh-ed25519 PINNED" {
		t.Errorf("pinned key = %q, want it untouched: a reset replaces nothing", got)
	}
}

// TestResetRefusesANodeWithoutAWritableLayer: on such a node the reboot would
// change nothing and report success, which is worse than refusing.
func TestResetRefusesANodeWithoutAWritableLayer(t *testing.T) {
	sandboxHome(t)
	d := &resetDialer{
		downFrom: 1 << 30, upFrom: 1 << 30,
		rootBefore: "ext4", rootAfter: "ext4", flagAfter: "gone",
	}
	c, node := newResetCluster(t, d)

	res := c.resetOne(context.Background(), node)

	if res.OK() {
		t.Fatal("resetOne accepted a node with no writable layer")
	}
	for _, want := range []string{"no writable layer", "flash it first"} {
		if !strings.Contains(res.Err.Error(), want) {
			t.Errorf("error %q does not mention %q", res.Err, want)
		}
	}
	if res.Overlay {
		t.Error("a node on a plain rootfs is reported as having an overlay")
	}
	if d.seen("push:"+FlagReset) || d.seen("reboot") {
		t.Error("a node that cannot be reset was armed or rebooted anyway")
	}
}

// TestResetReportsANodeThatLostItsOverlay: the agent falls back to booting
// the golden rootfs directly when it cannot mount the layer. The node is up,
// so nothing else notices — this is what says so.
func TestResetReportsANodeThatLostItsOverlay(t *testing.T) {
	sandboxHome(t)
	d := &resetDialer{
		downFrom: 2, upFrom: 4,
		rootBefore: "overlay", rootAfter: "ext4", flagAfter: "gone",
	}
	c, node := newResetCluster(t, d)

	res := c.resetOne(context.Background(), node)

	if res.OK() {
		t.Fatal("a node that came back without its overlay was reported as reset")
	}
	if !strings.Contains(res.Err.Error(), "not an overlay") {
		t.Errorf("error = %v, want it to say the overlay is gone", res.Err)
	}
	if res.Overlay {
		t.Error("Overlay stayed true for a node that came back on the bare rootfs")
	}
}

// TestResetReportsAFlagTheAgentNeverCleared: the agent clears the flag itself,
// so one still on the card means the node never ran the reset.
func TestResetReportsAFlagTheAgentNeverCleared(t *testing.T) {
	sandboxHome(t)
	d := &resetDialer{
		downFrom: 2, upFrom: 4,
		rootBefore: "overlay", rootAfter: "overlay", flagAfter: "present",
	}
	c, node := newResetCluster(t, d)

	res := c.resetOne(context.Background(), node)

	if res.OK() {
		t.Fatal("a node that never ran the reset was reported as reset")
	}
	if !strings.Contains(res.Err.Error(), "did not run the reset") {
		t.Errorf("error = %v", res.Err)
	}
}

func TestHasOverlayRoot(t *testing.T) {
	d := &resetDialer{}
	cases := map[string]bool{
		"overlay":   true,
		"overlayfs": true,
		"OVERLAY":   true,
		"ext4":      false,
		"":          false,
	}
	for answer, want := range cases {
		if got := hasOverlayRoot(&resetConn{d: d, root: answer}); got != want {
			t.Errorf("hasOverlayRoot(%q) = %v, want %v", answer, got, want)
		}
	}
}
