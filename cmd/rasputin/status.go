package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runStatus prints a read-only view of the cluster. It never changes
// anything, so it is safe to run at any point during a flash.
func runStatus(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: rasputin status [node...]\n\n"+
			"Probes every node in parallel and prints a table. Read-only.\n")
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
	c, err := cluster.New(cfg, nil)
	if err != nil {
		return err
	}
	selected, err := c.Resolver.Select(targets)
	if err != nil {
		return err
	}
	fmt.Print(cluster.StatusTable(c.Status(context.Background(), selected)))
	return nil
}
