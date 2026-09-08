package main

import (
	"context"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runStatus prints a read-only view of the cluster. It never changes
// anything, so it is safe to run at any point during a flash.
func runStatus(cfg *config.Config, out *output, args []string) error {
	fs := out.flagSet("status")
	fs.Usage = func() {
		out.printfErr("usage: rasputin status [flags] [node...]\n\n" +
			"Probes every node in parallel and prints a table. Read-only.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	targets := fs.Args()
	if len(targets) == 0 {
		targets = []string{cluster.AllNodes}
	}

	// Status is chatty by nature; keep the resolver quiet so the table is
	// the output, not a scroll of connection attempts.
	pw, err := sudoPassword(cfg)
	if err != nil {
		return err
	}
	c, err := cluster.NewWithSudo(cfg, pw, nil)
	if err != nil {
		return err
	}
	selected, err := c.Resolver.Select(targets)
	if err != nil {
		return err
	}
	rows := c.Status(context.Background(), selected)
	out.printf("%s", cluster.StatusTable(rows))
	return out.result("status", struct {
		Nodes []statusJSON `json:"nodes"`
	}{toStatusJSON(rows)}, nil)
}
