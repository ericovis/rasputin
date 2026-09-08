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
	// needsConfig is false only for `init`, which writes the config the
	// others load; such a command gets a Config with just Path filled in.
	needsConfig bool
	run         func(cfg *config.Config, args []string) error
}

// Readable values for command.needsConfig.
const (
	withConfig    = true
	withoutConfig = false
)

var commands = []command{
	{"init", "init [flags]", "write a starter rasputin.yaml to edit", withoutConfig, runInit},
	{"sync", "sync [flags]", "do whatever it takes to bring the cluster to the config", withConfig, runSync},
	{"prepare", "prepare", "build recovery.gz + vanilla image with custom boot partition", withConfig, runPrepare},
	{"adopt", "adopt <node|all>", "install the recovery mechanism on a live node via SSH", withConfig, runAdopt},
	{"dryrun", "dryrun <node|all>", "validate the download+decode pipeline on a node, harmlessly", withConfig, runDryrun},
	{"bake", "bake", "produce out/golden.img.zst using the builder node", withConfig, runBake},
	{"flash", "flash [flags] <node...|all>", "reflash node(s) from the golden image", withConfig, runFlash},
	{"status", "status", "table of node, ip, reachable, hostname, build-id, uptime", withConfig, runStatus},
	{"serve", "serve", "run the HTTP server standalone (debugging)", withConfig, runServe},
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
	cfg := &config.Config{Path: *cfgPath}
	if cmd.needsConfig {
		loaded, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		cfg = loaded
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
