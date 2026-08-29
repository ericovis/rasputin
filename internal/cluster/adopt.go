package cluster

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ericovis/rasputin/internal/bootfs"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/nodes"
	"github.com/ericovis/rasputin/internal/prepare"
)

// AdoptRebootTimeout is how long a node gets to come back after adoption.
// A Pi 3 boots in about a minute; ten leaves room for an fsck.
const AdoptRebootTimeout = 10 * time.Minute

// AdoptOptions configures an adoption.
type AdoptOptions struct {
	// RebootCheck reboots the node afterwards and proves the recovery
	// initramfs still lets it boot. On by default, because the alternative
	// is finding out during a flash.
	RebootCheck bool
}

// AdoptResult is what happened to one node.
type AdoptResult struct {
	Node      string
	Preflight nodes.Preflight
	Rebooted  bool
	Err       error
}

// OK reports whether the node was adopted successfully.
func (r AdoptResult) OK() bool { return r.Err == nil }

// Adopt installs the recovery mechanism on a live, stock node over SSH.
//
// This is how a cluster with no physical access gets onboarded: after it, a
// node can be reflashed remotely, because every boot from then on passes
// through the recovery agent. It deliberately does not flash anything.
func (c *Cluster) Adopt(ctx context.Context, node config.Node, opts AdoptOptions) AdoptResult {
	res := AdoptResult{Node: node.Name}

	recovery, err := os.ReadFile(prepare.RecoveryPath)
	if err != nil {
		res.Err = fmt.Errorf("reading %s (run `rasputin prepare` first): %w", prepare.RecoveryPath, err)
		return res
	}

	conn, err := c.Resolver.Connect(ctx, node)
	if err != nil {
		res.Err = err
		return res
	}
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()

	res.Preflight = nodes.RunPreflight(conn, node.Name, int64(len(recovery)))
	if !res.Preflight.OK() {
		res.Err = fmt.Errorf("%s failed preflight:\n%s", node.Name, res.Preflight)
		return res
	}

	// Back up the stock config.txt once, and only once: a second adoption
	// must not overwrite the pristine copy with an already-patched one.
	backup := ConfigPath + BackupSuffix
	if _, err := conn.Sudo(fmt.Sprintf("test -f %s", shellQuote(backup))); err != nil {
		c.Log("%s: backing up config.txt to %s", node.Name, backup)
		if _, err := conn.Sudo(fmt.Sprintf("cp -p %s %s", shellQuote(ConfigPath), shellQuote(backup))); err != nil {
			res.Err = fmt.Errorf("%s: backing up config.txt: %w", node.Name, err)
			return res
		}
	} else {
		c.Log("%s: %s already exists, keeping it", node.Name, backup)
	}

	c.Log("%s: installing recovery.gz (%d bytes)", node.Name, len(recovery))
	if err := conn.Push(RecoveryPath, recovery, "0644"); err != nil {
		res.Err = err
		return res
	}

	pairs := make([][2]string, 0, len(c.Cfg.Nodes))
	for _, n := range c.Cfg.Nodes {
		pairs = append(pairs, [2]string{n.MAC, n.Name})
	}
	if err := conn.Push(NodesPath, bootfs.NodesConf(pairs), "0644"); err != nil {
		res.Err = err
		return res
	}

	// Patch the node's own config.txt with the same function that patches an
	// image, so an adopted node and a freshly flashed one agree exactly.
	current, err := conn.Fetch(ConfigPath)
	if err != nil {
		res.Err = fmt.Errorf("%s: reading config.txt: %w", node.Name, err)
		return res
	}
	patched := bootfs.PatchConfigTxt(current)
	if err := conn.Push(ConfigPath, patched, "0644"); err != nil {
		res.Err = err
		return res
	}
	if _, err := conn.Sudo("sync"); err != nil {
		res.Err = fmt.Errorf("%s: sync: %w", node.Name, err)
		return res
	}
	c.Log("%s: recovery mechanism installed", node.Name)

	if !opts.RebootCheck {
		return res
	}

	back, err := c.RebootAndWait(ctx, node, conn, RebootOptions{Back: AdoptRebootTimeout})
	conn = nil // RebootAndWait closed it
	if err != nil {
		res.Err = err
		return res
	}
	conn = back
	res.Rebooted = true

	if err := c.verifyAdopted(conn, node); err != nil {
		res.Err = err
		return res
	}
	c.Log("%s: back up and booting through the recovery agent", node.Name)
	return res
}

// verifyAdopted proves the node came back with the recovery initramfs still
// configured, and that the agent left no complaints in the kernel log.
func (c *Cluster) verifyAdopted(conn nodes.Conn, node config.Node) error {
	cfg, err := conn.Fetch(ConfigPath)
	if err != nil {
		return fmt.Errorf("%s: reading config.txt after reboot: %w", node.Name, err)
	}
	if !strings.Contains(string(cfg), bootfs.InitramfsLine) {
		return fmt.Errorf("%s: config.txt no longer has %q after the reboot", node.Name, bootfs.InitramfsLine)
	}
	if strings.Contains(string(cfg), "auto_initramfs=") {
		return fmt.Errorf("%s: config.txt still sets auto_initramfs after the reboot", node.Name)
	}

	// The agent logs everything it does with a "rasputin:" prefix; a FATAL
	// there means it held instead of booting normally, which would be very
	// odd given we are talking to the booted system, but worth catching.
	dmesg, err := conn.Output("sudo -n dmesg | grep -i 'rasputin' || true")
	if err != nil {
		c.Log("%s: could not read dmesg: %v", node.Name, err)
		return nil
	}
	for _, line := range strings.Split(dmesg, "\n") {
		if strings.Contains(line, "FATAL") || strings.Contains(strings.ToLower(line), "error") {
			return fmt.Errorf("%s: the recovery agent logged a problem: %s", node.Name, strings.TrimSpace(line))
		}
	}
	if strings.TrimSpace(dmesg) != "" {
		c.Log("%s: agent log: %s", node.Name, firstLine(dmesg))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
