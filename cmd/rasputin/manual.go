package main

import (
	_ "embed"
	"fmt"

	"github.com/ericovis/rasputin/internal/config"
)

// manualText is the operator's manual, compiled into the binary so that a
// person or a program holding nothing but the executable can read it.
//
//go:embed MANUAL.md
var manualText string

// runManual prints the manual. It needs no config: reading the manual is
// how one learns to write the config.
func runManual(_ *config.Config, out *output, args []string) error {
	fs := out.flagSet("manual")
	fs.Usage = func() {
		out.printfErr("usage: rasputin manual [flags]\n\n" +
			"Prints the manual: what every command does, what it touches, what it\n" +
			"prints, and the JSON each one emits with -json.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("manual takes no arguments")
	}
	out.printf("%s", manualText)

	type cmdJSON struct {
		Name  string `json:"name"`
		Usage string `json:"usage"`
		Short string `json:"short"`
	}
	list := make([]cmdJSON, len(commands))
	for i, c := range commands {
		list[i] = cmdJSON{Name: c.name, Usage: "rasputin " + c.usage, Short: c.short}
	}
	return out.result("manual", struct {
		Manual   string    `json:"manual"`
		Commands []cmdJSON `json:"commands"`
	}{manualText, list}, nil)
}
