package main

import (
	_ "embed"
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cpuguy83/go-md2man/v2/md2man"

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
	man := fs.Bool("man", false, "print a man page (roff) instead of Markdown: `rasputin manual -man | man -l -`")
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

	var roff string
	if *man {
		roff = manPage()
		out.printf("%s", roff)
	} else {
		out.printf("%s", manualText)
	}

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
		Man      string    `json:"man,omitempty"`
		Commands []cmdJSON `json:"commands"`
	}{manualText, roff, list}, nil)
}

// manPage renders MANUAL.md as a section-1 man page. The Markdown is the
// single source; this only adds what man(7) expects and MANUAL.md, read as
// Markdown, does not want: the .TH header, NAME and SYNOPSIS sections, and
// upper-case, un-numbered section titles.
func manPage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# RASPUTIN 1 %q \"rasputin\" \"rasputin manual\"\n\n", manDate())
	b.WriteString("## NAME\n\nrasputin - reflash a Raspberry Pi cluster over the network\n\n")
	b.WriteString("## SYNOPSIS\n\n**rasputin** [**-c** *rasputin.yaml*] [**-json**] *command* [*flags*] [*args*]\n\n" +
		"**rasputin** *command* **-h**\n\n")
	b.WriteString("## DESCRIPTION\n\n")

	body := manualText
	if i := strings.Index(body, "\n"); i >= 0 && strings.HasPrefix(body, "# ") {
		body = body[i+1:] // the Markdown title; the .TH header replaces it
	}
	for _, line := range strings.Split(body, "\n") {
		if title, ok := strings.CutPrefix(line, "## "); ok {
			line = "## " + strings.ToUpper(sectionNumber.ReplaceAllString(title, ""))
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return string(md2man.Render([]byte(b.String())))
}

var sectionNumber = regexp.MustCompile(`^\d+\.\s+`)

// manDate is the date in the man page footer: the commit the binary was
// built from when Go recorded one (a real build inside the repository), so
// the page dates itself like the code; today otherwise (`go run`).
func manDate() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.time" && len(s.Value) >= 10 {
				return s.Value[:10]
			}
		}
	}
	return time.Now().Format("2006-01-02")
}
