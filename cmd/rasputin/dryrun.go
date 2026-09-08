package main

import (
	"context"
	"fmt"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runDryrun rehearses a reflash on real nodes without touching their cards.
func runDryrun(cfg *config.Config, out *output, args []string) error {
	fs := out.flagSet("dryrun")
	useVanilla := fs.Bool("vanilla", false, "rehearse with the prepared stock image even if a golden image exists")
	fs.Usage = func() {
		out.printfErr("usage: rasputin dryrun [flags] <node|all>\n\n" +
			"Runs the whole download-and-decode pipeline on a node, writing the\n" +
			"result to nowhere. The SD card is never opened for writing.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, targets, err := setup(cfg, out, fs.Args())
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
	out.logf("serving %s at %s", img.Path, srv.URLFor(img.Name))

	report := struct {
		Image  imageJSON        `json:"image"`
		Nodes  []dryrunNodeJSON `json:"nodes"`
		Passed int              `json:"passed"`
		Failed int              `json:"failed"`
	}{Image: imageJSON{Name: img.Name, Path: img.Path, URL: srv.URLFor(img.Name)}, Nodes: []dryrunNodeJSON{}}

	ctx := context.Background()
	for _, node := range targets {
		out.printf("\n=== %s ===\n", node.Name)
		res := c.Dryrun(ctx, srv, img, node)
		report.Nodes = append(report.Nodes, toDryrunJSON(res))
		if res.Report != "" {
			out.printf("%s\n", res.Report)
		}
		if res.OK() {
			report.Passed++
			out.printf("%s: PASS\n", node.Name)
			continue
		}
		report.Failed++
		out.printf("%s: FAIL — %v\n", node.Name, res.Err)
	}
	if report.Failed > 0 {
		return out.result("dryrun", report,
			fmt.Errorf("%d of %d node(s) failed the dryrun", report.Failed, len(targets)))
	}
	out.printf("\n%d node(s) passed the dryrun\n", len(targets))
	return out.result("dryrun", report, nil)
}
