package main

import (
	"context"
	"fmt"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runAdopt installs the recovery mechanism on live nodes, one at a time.
//
// Sequential on purpose: adoption reboots each node, and doing several at
// once would mean several nodes down simultaneously with no way to tell
// which one is in trouble.
func runAdopt(cfg *config.Config, out *output, args []string) error {
	fs := out.flagSet("adopt")
	rebootCheck := fs.Bool("reboot-check", true, "reboot each node afterwards and verify it boots through the recovery agent")
	fs.Usage = func() {
		out.printfErr("usage: rasputin adopt [flags] <node|all>\n\n" +
			"Installs recovery.gz and the config.txt hook on a live node over SSH.\n" +
			"Does not flash anything.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, targets, err := setup(cfg, out, fs.Args())
	if err != nil {
		return err
	}

	report := struct {
		RebootCheck bool            `json:"reboot_check"`
		Nodes       []adoptNodeJSON `json:"nodes"`
		Adopted     int             `json:"adopted"`
		Failed      int             `json:"failed"`
	}{RebootCheck: *rebootCheck, Nodes: []adoptNodeJSON{}}

	ctx := context.Background()
	for _, node := range targets {
		out.printf("\n=== %s ===\n", node.Name)
		res := c.Adopt(ctx, node, cluster.AdoptOptions{RebootCheck: *rebootCheck})
		report.Nodes = append(report.Nodes, toAdoptJSON(res))
		if len(res.Preflight.Checks) > 0 {
			out.printf("%s", res.Preflight)
		}
		if res.OK() {
			report.Adopted++
			out.printf("%s: PASS%s\n", node.Name, rebootedNote(res.Rebooted))
			continue
		}
		report.Failed++
		out.printf("%s: FAIL — %v\n", node.Name, res.Err)
	}
	if report.Failed > 0 {
		return out.result("adopt", report,
			fmt.Errorf("%d of %d node(s) could not be adopted", report.Failed, len(targets)))
	}
	out.printf("\nadopted %d node(s)\n", len(targets))
	return out.result("adopt", report, nil)
}

func rebootedNote(rebooted bool) string {
	if rebooted {
		return " (survived a reboot)"
	}
	return " (not reboot-checked)"
}

// setup builds the cluster and resolves the node arguments, the two things
// every node-touching command starts with.
func setup(cfg *config.Config, out *output, args []string) (*cluster.Cluster, []config.Node, error) {
	c, err := newCluster(cfg, out)
	if err != nil {
		return nil, nil, err
	}
	targets, err := c.Resolver.Select(args)
	if err != nil {
		return nil, nil, err
	}
	return c, targets, nil
}

// newCluster builds a Cluster, obtaining a sudo password first if the config
// asks for one. Its progress lines go through out, so in JSON mode they are
// {"type":"log"} objects rather than text on the JSON stream.
func newCluster(cfg *config.Config, out *output) (*cluster.Cluster, error) {
	pw, err := sudoPassword(cfg)
	if err != nil {
		return nil, err
	}
	return cluster.NewWithSudo(cfg, pw, out.logf)
}
