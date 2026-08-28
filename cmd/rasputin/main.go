// Command rasputin reflashes a Raspberry Pi cluster over the network.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ericovis/rasputin/internal/config"
)

// errNotImplemented marks a command that is planned but not built yet.
var errNotImplemented = errors.New("not implemented")

type command struct {
	name  string
	usage string
	short string
	// hidden keeps a command out of the help text. Used for build-time
	// helpers that are not part of the operator-facing surface.
	hidden bool
	run    func(cfg *config.Config, args []string) error
}

var commands = []command{
	{"prepare", "prepare", "build recovery.gz + vanilla image with custom boot partition", false, runPrepare},
	{"adopt", "adopt <node|all>", "install the recovery mechanism on a live node via SSH", false, notImplemented},
	{"dryrun", "dryrun <node|all>", "validate the download+decode pipeline on a node, harmlessly", false, notImplemented},
	{"bake", "bake", "produce out/golden.img.zst using the builder node", false, notImplemented},
	{"flash", "flash <node...|all>", "reflash node(s) from the golden image", false, notImplemented},
	{"status", "status", "table of node, ip, reachable, hostname, build-id, uptime", false, notImplemented},
	{"serve", "serve", "run the HTTP server standalone (debugging)", false, notImplemented},
	{"vanilla-fetch", "vanilla-fetch", "download and cache the stock Raspberry Pi OS image", true, runVanillaFetch},
}

func notImplemented(*config.Config, []string) error { return errNotImplemented }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "rasputin: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	fs := flag.NewFlagSet("rasputin", flag.ContinueOnError)
	cfgPath := fs.String("c", "./rasputin.yaml", "path to the cluster config")
	fs.Usage = func() { usage(fs.Output()) }
	if err := fs.Parse(argv); err != nil {
		return err
	}
	args := fs.Args()
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no command given")
	}
	cmd := lookup(args[0])
	if cmd == nil {
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	return cmd.run(cfg, args[1:])
}

func lookup(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "usage: rasputin [-c rasputin.yaml] <command> [args]\n\ncommands:\n")
	for _, c := range commands {
		if c.hidden {
			continue
		}
		fmt.Fprintf(w, "  %-20s %s\n", c.usage, c.short)
	}
}
