package cluster

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ericovis/rasputin/internal/config"
)

// KnownHostsPath is the file `ssh` consults, and which a reflash invalidates.
func KnownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

// PurgeKnownHosts removes a node's stale entries from the operator's own
// known_hosts after it has been reflashed.
//
// A reflashed node regenerates its SSH host keys by design, so every address
// it answers on now presents a different key. The CLI keeps its own pin in
// out/state.json and clears that itself — but a human typing `ssh rasputin001`
// would be met with REMOTE HOST IDENTIFICATION HAS CHANGED and a refusal.
// Clearing the entries here means the node just works afterwards.
//
// Best effort throughout: this is a convenience, and nothing about a
// successful flash should be reported as failed because a helper file could
// not be edited. ssh-keygen writes its own .old backup.
func (c *Cluster) PurgeKnownHosts(node config.Node, extra ...string) {
	path := KnownHostsPath()
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return // nothing to purge
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		c.Log("%s: ssh-keygen not found; run `ssh-keygen -R %s` yourself if ssh complains",
			node.Name, node.Name+".local")
		return
	}

	var purged []string
	for _, host := range c.knownHostCandidates(node, extra) {
		cmd := exec.Command(keygen, "-R", host)
		// ssh-keygen is chatty on success and exits non-zero when the host
		// is simply absent, which is the common case and not a problem.
		if out, err := cmd.CombinedOutput(); err == nil && strings.Contains(string(out), "Host found") {
			purged = append(purged, host)
		} else if err != nil && !errors.As(err, new(*exec.ExitError)) {
			c.Log("%s: could not run ssh-keygen -R %s: %v", node.Name, host, err)
		}
	}
	if len(purged) > 0 {
		c.Log("%s: cleared stale host keys from %s for %s (backup: %s.old)",
			node.Name, path, strings.Join(purged, ", "), path)
	}
}

// knownHostCandidates lists every address this node may be recorded under:
// its mDNS name, its bare name, and any address we have actually reached it
// on. Duplicates are collapsed.
func (c *Cluster) knownHostCandidates(node config.Node, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	add(node.Name)
	add(node.Name + ".local")
	if c.State != nil {
		if s, ok := c.State.Get(node.Name); ok {
			add(s.IP)
		}
	}
	for _, e := range extra {
		add(e)
	}
	return out
}
