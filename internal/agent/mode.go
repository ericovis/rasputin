// Package agent implements the rasputin recovery agent: the static binary
// that is the initramfs /init on every node.
//
// Everything that touches syscalls lives in linux-tagged files; the decision
// logic in this file is portable so it can be unit-tested on the build host.
package agent

import "strings"

// Mode is what this boot should do, decided by flag files on the FAT boot
// partition.
type Mode string

const (
	// ModeNormal hands over to the real system: no flag file was found.
	ModeNormal Mode = "normal"
	// ModeReflash overwrites the whole SD card from an image stream.
	ModeReflash Mode = "reflash"
	// ModeDryrun runs the reflash pipeline into a discard writer.
	ModeDryrun Mode = "dryrun"
	// ModeCapture streams the used part of the card back to the CLI.
	ModeCapture Mode = "capture"
	// ModeError means a flag was found but is unusable (no URL to work
	// with). The agent only logs in this mode — it never touches the card.
	ModeError Mode = "error"
)

// Flag file names on the boot partition, in decreasing priority. A dryrun
// must never lose to a reflash: the whole point of a dryrun is not wiping.
const (
	FlagDryrun  = "reflash-dryrun"
	FlagCapture = "capture"
	FlagReflash = "reflash"
)

// flagOrder is the precedence used by SelectMode.
var flagOrder = []struct {
	file string
	mode Mode
}{
	{FlagDryrun, ModeDryrun},
	{FlagCapture, ModeCapture},
	{FlagReflash, ModeReflash},
}

// SelectMode decides the boot mode from the flag files present on the boot
// partition. files maps a flag file's name to its contents; absent files must
// be absent from the map.
//
// The first non-empty line of a flag file overrides defaultURL, which is what
// lets the CLI point a node at its ephemeral HTTP server while a bare
// `touch /boot/firmware/reflash` still works against the baked-in default.
//
// A mode that needs a URL but has none resolves to ModeError, so a
// misconfigured node logs instead of wiping itself.
func SelectMode(files map[string]string, defaultURL string) (Mode, string) {
	for _, f := range flagOrder {
		content, ok := files[f.file]
		if !ok {
			continue
		}
		url := FirstLine(content)
		if url == "" {
			url = strings.TrimSpace(defaultURL)
		}
		if url == "" {
			return ModeError, ""
		}
		return f.mode, url
	}
	return ModeNormal, ""
}

// FirstLine returns the first line of s with surrounding whitespace removed.
// Flag files are written by shell one-liners and by the CLI, so they pick up
// stray newlines and carriage returns.
func FirstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// NeedsNetwork reports whether the mode talks to the CLI over HTTP.
func (m Mode) NeedsNetwork() bool {
	return m == ModeReflash || m == ModeDryrun || m == ModeCapture
}
