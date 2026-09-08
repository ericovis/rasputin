package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runBake produces the golden image using the builder node as an arm64
// build machine.
func runBake(cfg *config.Config, out *output, args []string) error {
	fs := out.flagSet("bake")
	fs.Usage = func() {
		out.printfErr("usage: rasputin bake [flags]\n\n"+
			"Reflashes the builder node (%s) with the prepared stock image, waits\n"+
			"for it to provision itself, seals it, and captures its card as\n"+
			"out/golden.img.zst. This wipes the builder.\n\nflags:\n", cfg.Builder)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("bake takes no arguments; the builder is %s from the config", cfg.Builder)
	}

	c, err := newCluster(cfg, out)
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
	out.printf("\ngolden image baked in %s\n", res.Duration.Round(time.Second))
	out.printf("  build:  %s\n", res.Meta.BuildID)
	out.printf("  base:   %s\n", res.Meta.Base)
	out.printf("  size:   %d bytes compressed, %d bytes of card\n", res.Meta.Bytes, res.Meta.CardUsed)
	out.printf("  sha256: %s\n", res.Meta.SHA256)
	out.printf("  meta:   %s\n", cluster.GoldenMetaPath)
	return out.result("bake", struct {
		DurationSeconds float64             `json:"duration_seconds"`
		Golden          *cluster.GoldenMeta `json:"golden"`
		GoldenPath      string              `json:"golden_path"`
		MetaPath        string              `json:"meta_path"`
	}{seconds(res.Duration), res.Meta, cluster.GoldenImage.Path, cluster.GoldenMetaPath}, nil)
}
