package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runAdopt installs the recovery mechanism on live nodes, one at a time.
//
// Sequential on purpose: adoption reboots each node, and doing several at
// once would mean several nodes down simultaneously with no way to tell
// which one is in trouble.
func runAdopt(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ContinueOnError)
	rebootCheck := fs.Bool("reboot-check", true, "reboot each node afterwards and verify it boots through the recovery agent")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: rasputin adopt [flags] <node|all>\n\n"+
			"Installs recovery.gz and the config.txt hook on a live node over SSH.\n"+
			"Does not flash anything.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, targets, err := setup(cfg, fs.Args())
	if err != nil {
		return err
	}

	ctx := context.Background()
	var failed int
	for _, node := range targets {
		fmt.Printf("\n=== %s ===\n", node.Name)
		res := c.Adopt(ctx, node, cluster.AdoptOptions{RebootCheck: *rebootCheck})
		if len(res.Preflight.Checks) > 0 {
			fmt.Print(res.Preflight)
		}
		if res.OK() {
			fmt.Printf("%s: PASS%s\n", node.Name, rebootedNote(res.Rebooted))
			continue
		}
		failed++
		fmt.Printf("%s: FAIL — %v\n", node.Name, res.Err)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d node(s) could not be adopted", failed, len(targets))
	}
	fmt.Printf("\nadopted %d node(s)\n", len(targets))
	return nil
}

func rebootedNote(rebooted bool) string {
	if rebooted {
		return " (survived a reboot)"
	}
	return " (not reboot-checked)"
}

// setup builds the cluster and resolves the node arguments, the two things
// every node-touching command starts with.
func setup(cfg *config.Config, args []string) (*cluster.Cluster, []config.Node, error) {
	c, err := cluster.New(cfg, func(format string, a ...any) { fmt.Printf(format+"\n", a...) })
	if err != nil {
		return nil, nil, err
	}
	targets, err := c.Resolver.Select(args)
	if err != nil {
		return nil, nil, err
	}
	return c, targets, nil
}
