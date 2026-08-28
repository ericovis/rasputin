package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/nodes"
	"github.com/ericovis/rasputin/internal/server"
)

// ProgressTick is how often a waiting command reports transfer progress.
const ProgressTick = 10 * time.Second

// DryrunTimeout is how long a node gets to download, decode and come back.
// A ~700 MB image over 100 Mbit ethernet on a Pi 3 takes a few minutes; the
// rest is margin for a slow boot.
const DryrunTimeout = 15 * time.Minute

// DryrunResult is what one node's rehearsal proved.
type DryrunResult struct {
	Node   string
	Report string
	Err    error
}

// OK reports whether the node completed the pipeline.
func (r DryrunResult) OK() bool { return r.Err == nil }

// Dryrun makes a node run the entire reflash pipeline — download, decode,
// checksum — writing the result to nowhere.
//
// It is the last safe moment to discover that the image, the network or the
// agent is broken: nothing is written to the card, and the node boots back
// into its existing system either way.
func (c *Cluster) Dryrun(ctx context.Context, srv *server.Server, img Image, node config.Node) DryrunResult {
	res := DryrunResult{Node: node.Name}

	conn, err := c.Resolver.Connect(ctx, node)
	if err != nil {
		res.Err = err
		return res
	}

	url := srv.URLFor(img.Name)
	c.Log("%s: arming a dryrun against %s", node.Name, url)
	if err := WriteFlag(conn, FlagDryrun, url); err != nil {
		conn.Close()
		res.Err = fmt.Errorf("%s: writing the dryrun flag: %w", node.Name, err)
		return res
	}

	back, err := c.rebootWatchingProgress(ctx, node, conn, srv, DryrunTimeout)
	if err != nil {
		res.Err = err
		return res
	}
	defer back.Close()

	report, err := RemoteFile(back, DryrunLog)
	if err != nil {
		res.Err = fmt.Errorf("%s: reading %s: %w", node.Name, DryrunLog, err)
		return res
	}
	res.Report = strings.TrimSpace(report)
	if res.Report == "" {
		res.Err = fmt.Errorf("%s: no dryrun report at %s — did the node boot into the recovery agent?", node.Name, DryrunLog)
		return res
	}
	if !strings.Contains(res.Report, "result: OK") {
		res.Err = fmt.Errorf("%s: dryrun failed:\n%s", node.Name, res.Report)
		return res
	}

	// The flag is removed by the agent; if it is still there the node never
	// ran the dryrun and this report is a stale one from a previous run.
	if out, err := back.Output("test -f " + shellQuote(FlagDryrun) + " && echo present || echo gone"); err == nil && strings.TrimSpace(out) == "present" {
		res.Err = fmt.Errorf("%s: the dryrun flag is still set; the report above may be stale", node.Name)
		return res
	}
	return res
}

// rebootWatchingProgress reboots a node and prints its download progress
// while waiting for it to come back, so a long transfer looks alive rather
// than hung.
func (c *Cluster) rebootWatchingProgress(ctx context.Context, node config.Node, conn nodes.Conn, srv *server.Server, timeout time.Duration) (nodes.Conn, error) {
	watch, stop := context.WithCancel(ctx)
	defer stop()
	go c.watchProgress(watch, node, srv)
	return c.RebootAndWait(ctx, node, conn, timeout)
}

// watchProgress logs a node's transfer every few seconds until cancelled.
func (c *Cluster) watchProgress(ctx context.Context, node config.Node, srv *server.Server) {
	ticker := time.NewTicker(ProgressTick)
	defer ticker.Stop()
	var last int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p, ok := srv.ProgressFor(node.MAC)
			if !ok || p.Bytes == last {
				continue
			}
			last = p.Bytes
			c.Log("%s: pulled %s", node.Name, ProgressLine(p))
		}
	}
}
