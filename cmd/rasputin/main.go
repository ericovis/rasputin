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

type command struct {
	name  string
	usage string
	short string
	run   func(cfg *config.Config, args []string) error
}

var commands = []command{
	{"prepare", "prepare", "build recovery.gz + vanilla image with custom boot partition", runPrepare},
	{"adopt", "adopt <node|all>", "install the recovery mechanism on a live node via SSH", runAdopt},
	{"dryrun", "dryrun <node|all>", "validate the download+decode pipeline on a node, harmlessly", runDryrun},
	{"bake", "bake", "produce out/golden.img.zst using the builder node", runBake},
	{"flash", "flash <node...|all>", "reflash node(s) from the golden image", runFlash},
	{"status", "status", "table of node, ip, reachable, hostname, build-id, uptime", runStatus},
	{"serve", "serve", "run the HTTP server standalone (debugging)", runServe},
}

func main() {
	err := run(os.Args[1:])
	switch {
	case err == nil:
		return
	case errors.Is(err, flag.ErrHelp):
		// The usage text has already been printed; asking for help is not
		// a failure.
		return
	default:
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
		fmt.Fprintf(w, "  %-20s %s\n", c.usage, c.short)
	}
}
