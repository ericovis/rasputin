package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runFlash reflashes nodes from the golden image, in parallel.
func runFlash(cfg *config.Config, out *output, args []string) error {
	fs := out.flagSet("flash")
	force := fs.Bool("force", false, "reflash a node even when it already runs the golden build")
	fs.Usage = func() {
		out.printfErr("usage: rasputin flash [flags] <node...|all>\n\n" +
			"Reflashes each node from out/golden.img.zst and verifies it comes\n" +
			"back with the golden build id and its own hostname. Nodes are\n" +
			"flashed in parallel.\n\n" +
			"A node already running the golden build is left untouched and\n" +
			"reported as SKIP; pass -force to reflash it anyway. Flags must\n" +
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

	c, targets, err := setup(cfg, out, fs.Args())
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
	url := srv.URLFor(cluster.GoldenImage.Name)
	out.logf("serving golden build %s (%d bytes) at %s", meta.BuildID, meta.Bytes, url)
	out.logf("flashing %d node(s), timeout %d minutes each", len(targets), cfg.Timeouts.FlashMinutes)
	out.printf("\n")

	results := c.Flash(context.Background(), srv, cluster.GoldenImage, meta, targets, cluster.FlashOptions{Force: *force})

	report := struct {
		Golden         goldenJSON      `json:"golden"`
		TimeoutMinutes int             `json:"timeout_minutes"`
		Force          bool            `json:"force"`
		Nodes          []flashNodeJSON `json:"nodes"`
		Flashed        int             `json:"flashed"`
		Skipped        int             `json:"skipped"`
		Failed         int             `json:"failed"`
	}{
		Golden:         goldenJSON{BuildID: meta.BuildID, Bytes: meta.Bytes, Path: cluster.GoldenImage.Path, URL: url},
		TimeoutMinutes: cfg.Timeouts.FlashMinutes,
		Force:          *force,
		Nodes:          []flashNodeJSON{},
	}

	out.printf("\n%-14s %-8s %-10s %s\n", "NODE", "RESULT", "TIME", "DETAIL")
	for _, r := range results {
		report.Nodes = append(report.Nodes, toFlashJSON(r))
		switch {
		case r.OK() && r.Skipped:
			report.Skipped++
			out.printf("%-14s %-8s %-10s %s\n", r.Node, "SKIP", r.Duration.Round(time.Second), "already on build "+r.BuildID)
		case r.OK():
			report.Flashed++
			out.printf("%-14s %-8s %-10s %s\n", r.Node, "PASS", r.Duration.Round(time.Second), "build "+r.BuildID)
		default:
			report.Failed++
			out.printf("%-14s %-8s %-10s %v\n", r.Node, "FAIL", r.Duration.Round(time.Second), r.Err)
		}
	}
	if report.Failed > 0 {
		return out.result("flash", report,
			fmt.Errorf("%d of %d node(s) failed to flash", report.Failed, len(results)))
	}
	return out.result("flash", report, nil)
}

// goldenJSON is the golden image as `flash` offers it.
type goldenJSON struct {
	BuildID string `json:"build_id"`
	Bytes   int64  `json:"bytes"`
	Path    string `json:"path"`
	URL     string `json:"url"`
}
