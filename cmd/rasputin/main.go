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
	// needsConfig is false only for `init` and `manual`, which run before a
	// config exists; such a command gets a Config with just Path filled in.
	needsConfig bool
	run         func(cfg *config.Config, out *output, args []string) error
}

// Readable values for command.needsConfig.
const (
	withConfig    = true
	withoutConfig = false
)

// commands is filled in init rather than declared with its value: `manual`
// lists the commands, and a package-level initializer that referred to a
// function referring back to it would be an initialization cycle.
var commands []command

func init() {
	commands = []command{
		{"init", "init [flags]", "write a starter rasputin.yaml to edit", withoutConfig, runInit},
		{"sync", "sync [flags]", "do whatever it takes to bring the cluster to the config", withConfig, runSync},
		{"prepare", "prepare [flags]", "build recovery.gz + vanilla image with custom boot partition", withConfig, runPrepare},
		{"adopt", "adopt [flags] <node|all>", "install the recovery mechanism on a live node via SSH", withConfig, runAdopt},
		{"dryrun", "dryrun [flags] <node|all>", "validate the download+decode pipeline on a node, harmlessly", withConfig, runDryrun},
		{"bake", "bake [flags]", "produce out/golden.img.zst using the builder node", withConfig, runBake},
		{"flash", "flash [flags] <node...|all>", "reflash node(s) from the golden image", withConfig, runFlash},
		{"status", "status [flags] [node...]", "table of node, ip, reachable, hostname, build-id, uptime", withConfig, runStatus},
		{"serve", "serve [flags]", "run the HTTP server standalone (debugging)", withConfig, runServe},
		{"manual", "manual [flags]", "print the operator's manual (what every command does, and its JSON)", withoutConfig, runManual},
	}
}

func main() {
	err := run(os.Args[1:], stdOutput())
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

func run(argv []string, out *output) error {
	fs := out.flagSet("rasputin")
	cfgPath := fs.String("c", "./rasputin.yaml", "path to the cluster config")
	fs.Usage = func() { usage(fs.Output()) }
	if err := fs.Parse(argv); err != nil {
		return err
	}
	args := fs.Args()
	if len(args) == 0 {
		usage(out.errw)
		return errors.New("no command given")
	}
	name, rest := args[0], args[1:]
	// A -json after the command name is the command's flag, parsed later;
	// it has to be known now so that a config that does not load is still
	// reported in the form the caller asked for.
	if hasJSONFlag(rest) {
		out.json = true
	}
	cmd := lookup(name)
	if cmd == nil {
		usage(out.errw)
		return out.result(name, nil, fmt.Errorf("unknown command %q", name))
	}
	cfg := &config.Config{Path: *cfgPath}
	if cmd.needsConfig {
		loaded, err := config.Load(*cfgPath)
		if err != nil {
			return out.result(cmd.name, nil, err)
		}
		cfg = loaded
	}
	err := cmd.run(cfg, out, rest)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		// A no-op when the command already wrote its own result object.
		return out.result(cmd.name, nil, err)
	}
	return err
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
	fmt.Fprintf(w, "usage: rasputin [-c rasputin.yaml] [-json] <command> [flags] [args]\n\ncommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-28s %s\n", c.usage, c.short)
	}
	fmt.Fprintf(w, "\nEvery command takes -json (newline-delimited JSON on stdout, last line\n"+
		"{\"type\":\"result\",...}). `rasputin manual` is the full manual.\n")
}
