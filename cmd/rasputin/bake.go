package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runBake produces the golden image using the builder node as an arm64
// build machine.
func runBake(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("bake", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: rasputin bake\n\n"+
			"Reflashes the builder node (%s) with the prepared stock image, waits\n"+
			"for it to provision itself, seals it, and captures its card as\n"+
			"out/golden.img.zst. This wipes the builder.\n", cfg.Builder)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("bake takes no arguments; the builder is %s from the config", cfg.Builder)
	}

	c, err := cluster.New(cfg, func(format string, a ...any) { fmt.Printf(format+"\n", a...) })
	if err != nil {
		return err
	}
	srv, err := c.Serve()
	if err != nil {
		return err
	}
	defer srv.Close()

	res, err := c.Bake(context.Background(), srv)
	if err != nil {
		return err
	}
	fmt.Printf("\ngolden image baked in %s\n", res.Duration.Round(time.Second))
	fmt.Printf("  build:  %s\n", res.Meta.BuildID)
	fmt.Printf("  base:   %s\n", res.Meta.Base)
	fmt.Printf("  size:   %d bytes compressed, %d bytes of card\n", res.Meta.Bytes, res.Meta.CardUsed)
	fmt.Printf("  sha256: %s\n", res.Meta.SHA256)
	fmt.Printf("  meta:   %s\n", cluster.GoldenMetaPath)
	return nil
}
