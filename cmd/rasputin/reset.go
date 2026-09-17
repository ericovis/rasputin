package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
)

// runReset throws away what nodes have written since they were flashed.
//
// It is the command for the loop the cluster exists to serve: try something
// on four Pis, put them back exactly as the golden image left them, try the
// next thing. A flash would do the same in six minutes and a whole-card
// write; this does it in seconds, because the golden rootfs is a read-only
// layer that never changed in the first place.
func runReset(cfg *config.Config, out *output, args []string) error {
	fs := out.flagSet("reset")
	yes := fs.Bool("yes", false, "do not ask before wiping a node's writable layer")
	fs.Usage = func() {
		out.printfErr("usage: rasputin reset [flags] <node...|all>\n\n" +
			"Wipes each node's writable layer, putting it back to the golden\n" +
			"image it was flashed with. Everything written since then is lost.\n" +
			"Nothing else changes: no image crosses the wire, the card is not\n" +
			"rewritten, and the node keeps its SSH host keys — so it takes one\n" +
			"reboot rather than the six minutes of a flash.\n\n" +
			"A node whose golden image predates the writable layer is refused;\n" +
			"flash it once and it can be reset from then on. Flags must come\n" +
			"before the node list.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		// Which nodes to reset is the whole command; show what to type.
		fs.Usage()
	}

	c, targets, err := setup(cfg, out, fs.Args())
	if err != nil {
		return err
	}
	if !*yes {
		ok, err := confirmReset(out, os.Stdin, targets)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("cancelled; nothing was touched")
		}
	}

	report := resetReport(out, c.Reset(context.Background(), targets))
	if report.Failed > 0 {
		return out.result("reset", report,
			fmt.Errorf("%d of %d node(s) failed to reset", report.Failed, len(report.Nodes)))
	}
	return out.result("reset", report, nil)
}

// confirmReset asks before a reset destroys what the nodes have written. The
// question names the nodes, because a reset is not the flash an operator is
// used to confirming: it looks harmless right up to the moment the work of
// the last hour is gone.
func confirmReset(out *output, in *os.File, targets []config.Node) (bool, error) {
	names := make([]string, len(targets))
	for i, n := range targets {
		names[i] = n.Name
	}
	subject := fmt.Sprintf("this wipes everything %s wrote since the last reset",
		strings.Join(names, " "))
	if out.json {
		return false, fmt.Errorf("%s and -json never prompts: re-run with -yes", subject)
	}
	if in == nil || !term.IsTerminal(int(in.Fd())) {
		return false, fmt.Errorf("%s and there is no terminal to confirm on: re-run with -yes", subject)
	}
	out.printf("reset will wipe the writable layer of: %s\n", strings.Join(names, " "))
	return readYesNo(out, in)
}

// resetReportJSON is the fields of the `reset` result object.
type resetReportJSON struct {
	Nodes  []resetNodeJSON `json:"nodes"`
	Reset  int             `json:"reset"`
	Failed int             `json:"failed"`
}

// resetReport prints the per-node table and collects the same outcomes for a
// program reading -json.
func resetReport(out *output, results []cluster.ResetResult) resetReportJSON {
	report := resetReportJSON{Nodes: []resetNodeJSON{}}
	out.printf("\n%-14s %-8s %-10s %s\n", "NODE", "RESULT", "TIME", "DETAIL")
	for _, r := range results {
		report.Nodes = append(report.Nodes, toResetJSON(r))
		if r.OK() {
			report.Reset++
			out.printf("%-14s %-8s %-10s %s\n", r.Node, "PASS", r.Duration.Round(time.Second),
				"back on the golden image")
			continue
		}
		report.Failed++
		out.printf("%-14s %-8s %-10s %v\n", r.Node, "FAIL", r.Duration.Round(time.Second), r.Err)
	}
	return report
}
