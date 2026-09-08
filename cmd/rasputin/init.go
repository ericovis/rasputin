package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/ericovis/rasputin/internal/config"
)

// nodeList collects repeated -node name=mac flags in the order they are given.
type nodeList []config.Node

func (l *nodeList) String() string {
	names := make([]string, len(*l))
	for i, n := range *l {
		names[i] = n.Name
	}
	return strings.Join(names, ",")
}

func (l *nodeList) Set(v string) error {
	name, mac, ok := strings.Cut(v, "=")
	if !ok || name == "" || mac == "" {
		return fmt.Errorf("want name=mac, got %q", v)
	}
	parsed, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("node %s: bad mac %q: %w", name, mac, err)
	}
	*l = append(*l, config.Node{Name: name, MAC: strings.ToLower(parsed.String())})
	return nil
}

// runInit writes a starter rasputin.yaml. It is the one command that runs
// without a config, so cfg carries only the -c path.
func runInit(cfg *config.Config, args []string) error {
	return initConfig(os.Stdout, cfg.Path, args)
}

func initConfig(out io.Writer, path string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(out)
	force := fs.Bool("force", false, "overwrite an existing config")
	builder := fs.String("builder", "", "node that bakes the golden image (default: the first node)")
	var nodes nodeList
	fs.Var(&nodes, "node", "a node as name=mac; repeat for each Pi (default: four placeholders)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: rasputin [-c rasputin.yaml] init [flags]\n\n"+
			"Writes a commented starter config to the -c path. Without -node it\n"+
			"writes four placeholder nodes; every command that touches a node\n"+
			"refuses to run until the placeholder MACs are replaced with the\n"+
			"real ones from `cat /sys/class/net/eth0/address`.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("init takes no arguments, got %q", rest[0])
	}
	if path == "" {
		return fmt.Errorf("init: no config path; pass -c before the command")
	}

	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s already exists: pass -force to overwrite it", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}

	cfgNodes := nodes
	if len(cfgNodes) == 0 {
		cfgNodes = config.PlaceholderNodes(config.DefaultNodeCount)
	}
	// A placeholder config cannot be parsed by design, so this check happens
	// here rather than in config.Parse below: without it, `init -builder
	// typo` writes a file that only fails much later, once the real MACs
	// have been typed in.
	if *builder != "" && !hasNode(cfgNodes, *builder) {
		return fmt.Errorf("builder %q is not a known node; the nodes are %s",
			*builder, strings.Join(nodeNames(cfgNodes), " "))
	}

	data, err := config.RenderTemplate(config.TemplateOptions{Builder: *builder, Nodes: nodes})
	if err != nil {
		return err
	}
	// A config with real MACs must be loadable straight away; catch a bad
	// -builder or a duplicate MAC before it lands on disk. A placeholder
	// config cannot parse by design, so it is not checked.
	placeholder := true
	if len(nodes) > 0 {
		placeholder = false
		for _, n := range nodes {
			if config.IsPlaceholderMAC(n.MAC) {
				placeholder = true
			}
		}
	}
	if !placeholder {
		if _, err := config.Parse(data, path); err != nil {
			return fmt.Errorf("init would write an invalid config: %w", err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}

	names := nodeNames(cfgNodes)
	who := *builder
	if who == "" {
		who = cfgNodes[0].Name
	}
	fmt.Fprintf(out, "wrote %s: %d node(s) %s, builder %s\n",
		path, len(cfgNodes), strings.Join(names, " "), who)
	fmt.Fprintf(out, "\nnext:\n")
	if placeholder {
		fmt.Fprintf(out, "  $EDITOR %s   # replace the placeholder MACs "+
			"(cat /sys/class/net/eth0/address on each Pi)\n", path)
	} else {
		fmt.Fprintf(out, "  $EDITOR %s   # check the user, packages and rootfs cap\n", path)
	}
	fmt.Fprintf(out, "  rasputin sync          # prepare, bake and flash the whole cluster\n")
	return nil
}

// hasNode reports whether name is one of the nodes the config will carry.
func hasNode(nodes []config.Node, name string) bool {
	for _, n := range nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

func nodeNames(nodes []config.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name
	}
	return out
}
