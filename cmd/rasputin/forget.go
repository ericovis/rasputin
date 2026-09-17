package main

import (
	"fmt"
	"strings"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/state"
)

// statePath is the cache `forget` edits. A variable so the tests can point it
// at a temporary file instead of the repository's out/.
var statePath = state.DefaultPath

// runForget drops nodes' pinned SSH host keys.
//
// The pin is trust on first use, and the CLI drops it itself whenever it is
// the one replacing a node's system. When somebody else's machine did the
// reflashing, this machine's pins are stale and every connection is refused
// — correctly, since from here that looks exactly like an impersonation.
// This is the operator saying "it was a reflash, I know".
//
// It deliberately does not go through the cluster: a stale pin must be
// droppable without SSH credentials, a sudo password or a reachable node.
func runForget(cfg *config.Config, out *output, args []string) error {
	fs := out.flagSet("forget")
	fs.Usage = func() {
		out.printfErr("usage: rasputin forget [flags] <node...|all>\n\n"+
			"Drops each node's pinned SSH host key from %s, so the next\n"+
			"connection trusts the key the node presents and pins that one instead.\n"+
			"Touches no node and keeps the cached address. Use it after the nodes\n"+
			"were reflashed from another machine.\n\nflags:\n", statePath)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		// Which nodes to forget is the whole command; show what to type.
		fs.Usage()
	}
	targets, err := forgetTargets(cfg, fs.Args())
	if err != nil {
		return err
	}
	st, err := state.Load(statePath)
	if err != nil {
		return err
	}

	report := struct {
		Nodes     []forgetNodeJSON `json:"nodes"`
		Forgotten int              `json:"forgotten"`
	}{Nodes: []forgetNodeJSON{}}
	for _, name := range targets {
		if st.HostKey(name) == "" {
			out.printf("%s: no pinned host key\n", name)
			report.Nodes = append(report.Nodes, forgetNodeJSON{Node: name})
			continue
		}
		if err := st.ForgetHostKey(name); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		out.printf("%s: forgot pinned host key\n", name)
		report.Nodes = append(report.Nodes, forgetNodeJSON{Node: name, Forgotten: true})
		report.Forgotten++
	}
	return out.result("forget", report, nil)
}

// forgetTargets resolves the arguments against the config alone: names and
// `all`, nothing that needs the network or the cache.
func forgetTargets(cfg *config.Config, args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("no node given (use a node name, or %q)", cluster.AllNodes)
	}
	chosen := map[string]bool{}
	for _, arg := range args {
		if strings.EqualFold(arg, cluster.AllNodes) {
			for _, name := range cfg.NodeNames() {
				chosen[name] = true
			}
			continue
		}
		n := cfg.Node(arg)
		if n == nil {
			return nil, fmt.Errorf("%q is not a node in %s (known: %s, or %q)",
				arg, cfg.Path, strings.Join(cfg.NodeNames(), " "), cluster.AllNodes)
		}
		chosen[n.Name] = true
	}
	var out []string
	for _, name := range cfg.NodeNames() {
		if chosen[name] {
			out = append(out, name)
		}
	}
	return out, nil
}
