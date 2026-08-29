package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runFlash reflashes nodes from the golden image, in parallel.
func runFlash(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("flash", flag.ContinueOnError)
	force := fs.Bool("force", false, "reflash a node even when it already runs the golden build")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: rasputin flash [flags] <node...|all>\n\n"+
			"Reflashes each node from out/golden.img.zst and verifies it comes\n"+
			"back with the golden build id and its own hostname. Nodes are\n"+
			"flashed in parallel.\n\n"+
			"A node already running the golden build is left untouched and\n"+
			"reported as SKIP; pass -force to reflash it anyway. Flags must\n"+
			"come before the node list.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	meta, err := cluster.ReadGoldenMeta()
	if err != nil {
		return err
	}
	if !cluster.GoldenImage.Exists() {
		return fmt.Errorf("%s is missing: run `rasputin bake` first", cluster.GoldenImage.Path)
	}

	c, targets, err := setup(cfg, fs.Args())
	if err != nil {
		return err
	}

	srv, err := c.Serve()
	if err != nil {
		return err
	}
	defer srv.Close()
	if err := srv.Register(cluster.GoldenImage.Name, cluster.GoldenImage.Path); err != nil {
		return err
	}
	fmt.Printf("serving golden build %s (%d bytes) at %s\n",
		meta.BuildID, meta.Bytes, srv.URLFor(cluster.GoldenImage.Name))
	fmt.Printf("flashing %d node(s), timeout %d minutes each\n\n", len(targets), cfg.Timeouts.FlashMinutes)

	results := c.Flash(context.Background(), srv, cluster.GoldenImage, meta, targets, cluster.FlashOptions{Force: *force})

	fmt.Printf("\n%-14s %-8s %-10s %s\n", "NODE", "RESULT", "TIME", "DETAIL")
	var failed int
	for _, r := range results {
		if r.OK() {
			verdict, detail := "PASS", "build "+r.BuildID
			if r.Skipped {
				verdict, detail = "SKIP", "already on build "+r.BuildID
			}
			fmt.Printf("%-14s %-8s %-10s %s\n", r.Node, verdict,
				r.Duration.Round(time.Second), detail)
			continue
		}
		failed++
		fmt.Printf("%-14s %-8s %-10s %v\n", r.Node, "FAIL", r.Duration.Round(time.Second), r.Err)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d node(s) failed to flash", failed, len(results))
	}
	return nil
}
