package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runDryrun rehearses a reflash on real nodes without touching their cards.
func runDryrun(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("dryrun", flag.ContinueOnError)
	useVanilla := fs.Bool("vanilla", false, "rehearse with the prepared stock image even if a golden image exists")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: rasputin dryrun [flags] <node|all>\n\n"+
			"Runs the whole download-and-decode pipeline on a node, writing the\n"+
			"result to nowhere. The SD card is never opened for writing.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, targets, err := setup(cfg, fs.Args())
	if err != nil {
		return err
	}

	img := cluster.VanillaImage
	if !*useVanilla {
		if img, err = cluster.BestImage(); err != nil {
			return err
		}
	} else if !img.Exists() {
		return fmt.Errorf("no prepared image at %s: run `rasputin prepare` first", img.Path)
	}

	srv, err := c.Serve()
	if err != nil {
		return err
	}
	defer srv.Close()
	if err := srv.Register(img.Name, img.Path); err != nil {
		return err
	}
	fmt.Printf("serving %s at %s\n", img.Path, srv.URLFor(img.Name))

	ctx := context.Background()
	var failed int
	for _, node := range targets {
		fmt.Printf("\n=== %s ===\n", node.Name)
		res := c.Dryrun(ctx, srv, img, node)
		if res.Report != "" {
			fmt.Println(res.Report)
		}
		if res.OK() {
			fmt.Printf("%s: PASS\n", node.Name)
			continue
		}
		failed++
		fmt.Printf("%s: FAIL — %v\n", node.Name, res.Err)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d node(s) failed the dryrun", failed, len(targets))
	}
	fmt.Printf("\n%d node(s) passed the dryrun\n", len(targets))
	return nil
}
