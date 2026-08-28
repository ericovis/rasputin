package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/nodes"
	"github.com/ericovis/rasputin/internal/server"
)

// GoldenMetaPath records which golden image out/golden.img.zst is.
const GoldenMetaPath = "out/meta/golden.json"

// GoldenMeta is the provenance of a baked image.
type GoldenMeta struct {
	BuildID  string    `json:"build_id"`
	SHA256   string    `json:"sha256"`
	Bytes    int64     `json:"bytes"`
	Base     string    `json:"base"`
	Builder  string    `json:"builder"`
	BakedAt  time.Time `json:"baked_at"`
	CardUsed int64     `json:"card_used_bytes"`
}

// ReadGoldenMeta loads the golden image's provenance.
func ReadGoldenMeta() (*GoldenMeta, error) {
	raw, err := os.ReadFile(GoldenMetaPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s (run `rasputin bake` first): %w", GoldenMetaPath, err)
	}
	var m GoldenMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", GoldenMetaPath, err)
	}
	return &m, nil
}

// WriteGoldenMeta records a freshly baked image.
func WriteGoldenMeta(m *GoldenMeta) error {
	if err := os.MkdirAll("out/meta", 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(GoldenMetaPath, append(raw, '\n'), 0o644)
}

// FlashResult is one node's outcome.
type FlashResult struct {
	Node     string
	Duration time.Duration
	BuildID  string
	Hostname string
	Err      error
}

// OK reports whether the node came back as a healthy clone.
func (r FlashResult) OK() bool { return r.Err == nil }

// Flash reflashes nodes from the golden image, in parallel.
//
// Parallel is safe here in a way adoption is not: each node is independent,
// the whole point is to rebuild several at once, and a node that fails is
// left in the recovery agent retrying rather than half-broken.
func (c *Cluster) Flash(ctx context.Context, srv *server.Server, img Image, meta *GoldenMeta, targets []config.Node) []FlashResult {
	results := make([]FlashResult, len(targets))
	var wg sync.WaitGroup
	for i, node := range targets {
		wg.Add(1)
		go func(i int, node config.Node) {
			defer wg.Done()
			results[i] = c.flashOne(ctx, srv, img, meta, node)
		}(i, node)
	}
	wg.Wait()
	return results
}

func (c *Cluster) flashOne(ctx context.Context, srv *server.Server, img Image, meta *GoldenMeta, node config.Node) FlashResult {
	start := time.Now()
	res := FlashResult{Node: node.Name}

	conn, err := c.Resolver.Connect(ctx, node)
	if err != nil {
		res.Err = err
		return res
	}

	url := srv.URLFor(img.Name)
	c.Log("%s: arming a reflash from %s", node.Name, url)
	if err := WriteFlag(conn, FlagReflash, url); err != nil {
		conn.Close()
		res.Err = fmt.Errorf("%s: writing the reflash flag: %w", node.Name, err)
		return res
	}

	// The node is about to replace its whole filesystem, host keys and all.
	// Forgetting the recorded key now is what stops the next connection
	// looking like an impersonation.
	if err := c.State.ForgetHostKey(node.Name); err != nil {
		c.Log("%s: could not clear the recorded host key: %v", node.Name, err)
	}

	timeout := time.Duration(c.Cfg.Timeouts.FlashMinutes) * time.Minute
	back, err := c.rebootWatchingProgress(ctx, node, conn, srv, timeout)
	if err != nil {
		res.Err = err
		res.Duration = time.Since(start)
		return res
	}
	defer back.Close()

	res.Duration = time.Since(start)
	if err := c.verifyClone(back, node, meta, &res); err != nil {
		res.Err = err
	}
	return res
}

// verifyClone proves the node came back as the image we sent, with its own
// identity applied.
func (c *Cluster) verifyClone(conn nodes.Conn, node config.Node, meta *GoldenMeta, res *FlashResult) error {
	release, err := conn.Output("cat " + ReleaseFile)
	if err != nil {
		return fmt.Errorf("%s: no %s — the node did not come back from the golden image: %w", node.Name, ReleaseFile, err)
	}
	res.BuildID = buildIDFrom(release)
	if meta != nil && res.BuildID != meta.BuildID {
		return fmt.Errorf("%s: build id is %q, want the golden image's %q", node.Name, res.BuildID, meta.BuildID)
	}

	res.Hostname, err = conn.Output("hostname")
	if err != nil {
		return fmt.Errorf("%s: cannot read its hostname: %w", node.Name, err)
	}
	if res.Hostname != node.Name {
		return fmt.Errorf("%s: came back calling itself %q — the identity service did not apply nodes.conf",
			node.Name, res.Hostname)
	}

	// is-system-running exits non-zero for "degraded", which is normal on a
	// first boot where an optional unit failed, so the text is what counts.
	state, _ := conn.Output("systemctl is-system-running")
	state = strings.TrimSpace(state)
	switch state {
	case "running", "degraded":
	default:
		return fmt.Errorf("%s: systemd reports %q, want running or degraded", node.Name, state)
	}
	return nil
}
