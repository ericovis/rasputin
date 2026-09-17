package cluster

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/nodes"
)

// ResetTimeout is how long a node gets to come back from a reset. A reset is
// one reboot, not a flash: the agent empties the writable layer inside the
// initramfs and hands straight over to the system, so the node is back as
// soon as it has booted. Four minutes is a Pi 3's boot with room to spare.
const ResetTimeout = 4 * time.Minute

// overlayProbe asks a node what its root filesystem is. findmnt is on
// Raspberry Pi OS; stat is the fallback for a stripped-down image.
const overlayProbe = "findmnt -no FSTYPE / 2>/dev/null || stat -f -c %T / 2>/dev/null"

// ResetResult is one node's outcome. Overlay says the node was running on a
// writable layer, which is the precondition for a reset and, afterwards, the
// proof that it came back on one.
type ResetResult struct {
	Node     string
	Duration time.Duration
	Overlay  bool
	Err      error
}

// OK reports whether the node came back on a pristine writable layer.
func (r ResetResult) OK() bool { return r.Err == nil }

// Reset throws away everything each node has written since it was flashed or
// last reset, in parallel.
//
// It is the cheap counterpart of Flash: no image crosses the wire, no card is
// rewritten, and the node comes back as itself, with its pinned key and the
// operator's known_hosts both still valid — the agent carries the host keys
// and the machine-id across the wipe (agent.IdentityGlobs). Nothing here may
// touch either.
func (c *Cluster) Reset(ctx context.Context, targets []config.Node) []ResetResult {
	results := make([]ResetResult, len(targets))
	var wg sync.WaitGroup
	for i, node := range targets {
		wg.Add(1)
		go func(i int, node config.Node) {
			defer wg.Done()
			results[i] = c.resetOne(ctx, node)
		}(i, node)
	}
	wg.Wait()
	return results
}

func (c *Cluster) resetOne(ctx context.Context, node config.Node) ResetResult {
	start := time.Now()
	res := ResetResult{Node: node.Name}

	conn, err := c.Resolver.Connect(ctx, node)
	if err != nil {
		res.Err = err
		return res
	}
	// A node with no writable layer would boot, find nothing to empty and
	// come back exactly as it was, which looks like a successful reset and is
	// not one. Refuse before the reboot rather than lie about it afterwards.
	if !hasOverlayRoot(conn) {
		conn.Close()
		res.Duration = time.Since(start)
		res.Err = fmt.Errorf("%s has no writable layer (its golden predates overlay support): flash it first", node.Name)
		return res
	}
	res.Overlay = true

	c.Log("%s: arming a reset of the writable layer", node.Name)
	if err := WriteFlag(conn, FlagReset, ""); err != nil {
		conn.Close()
		res.Duration = time.Since(start)
		res.Err = fmt.Errorf("%s: writing the reset flag: %w", node.Name, err)
		return res
	}

	// ReplacesSystem is deliberately off: the system that comes back is the
	// same install with the same host keys, so dropping the pin would only
	// throw away the proof of that.
	back, err := c.RebootAndWait(ctx, node, conn, RebootOptions{Back: ResetTimeout})
	if err != nil {
		res.Duration = time.Since(start)
		res.Err = err
		return res
	}
	defer back.Close()
	res.Duration = time.Since(start)

	if !hasOverlayRoot(back) {
		res.Overlay = false
		res.Err = fmt.Errorf("%s: / is not an overlay any more — the agent could not mount the writable layer "+
			"and booted the golden rootfs directly", node.Name)
		return res
	}
	// The agent clears the flag itself. One still on the boot partition means
	// the node never reached the agent, so whatever it is running now is what
	// it was running before.
	if out, err := back.Output("test -f " + shellQuote(FlagReset) + " && echo present || echo gone"); err == nil &&
		strings.TrimSpace(out) == "present" {
		res.Err = fmt.Errorf("%s: the reset flag is still set; the node did not run the reset", node.Name)
		return res
	}
	return res
}

// hasOverlayRoot reports whether a node's root filesystem is the overlay of
// the golden image and its writable layer. Anything else — an unreadable
// answer included — is a no: the caller refuses rather than resets blind.
func hasOverlayRoot(conn nodes.Conn) bool {
	out, err := conn.Output(overlayProbe)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "overlay", "overlayfs":
			return true
		}
	}
	return false
}
