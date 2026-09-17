package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/ericovis/rasputin/internal/card"
	"github.com/ericovis/rasputin/internal/cluster"
	"github.com/ericovis/rasputin/internal/config"
	"github.com/ericovis/rasputin/internal/events"
	"github.com/ericovis/rasputin/internal/tui"
)

// The artifacts write-card reads. Variables so the tests can point them at a
// temporary directory instead of the repository's out/.
var (
	goldenPath     = cluster.GoldenImage.Path
	vanillaPath    = cluster.VanillaImage.Path
	goldenMetaPath = cluster.GoldenMetaPath
)

// writeCardStep names the transfer on the JSON stream. It is not one of
// `sync`'s steps — nothing plans a card write — but a program watching bytes
// move wants them in the shape it already parses.
const writeCardStep events.StepID = "write-card"

// runWriteCard writes an image to a card in this machine's own reader.
//
// It is the way out of the two situations the network cannot reach: day 0,
// when there is no node to talk to yet, and a node whose card has died and
// needs a new one. Everything else about the cluster stays remote.
func runWriteCard(_ *config.Config, out *output, args []string) error {
	fs := out.flagSet("write-card")
	device := fs.String("device", "", "the disk to write, e.g. `/dev/disk4` (or a file to export the image to); without it, pick one from a list")
	image := fs.String("image", imageGolden, "which image to write: `golden` (a ready node) or vanilla (a node that provisions itself)")
	yes := fs.Bool("yes", false, "do not ask before erasing the disk")
	fs.Usage = func() {
		out.printfErr("usage: rasputin write-card [flags]\n\n" +
			"Writes an image to an SD card in this machine's card reader, for a\n" +
			"card that cannot be written over the network: the first card of a\n" +
			"new node, or a replacement for one that died.\n\n" +
			"Everything on the chosen disk is erased. Without -device the\n" +
			"command shows the removable disks it can find and asks which one;\n" +
			"with -json it refuses to guess and wants -device and -yes.\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("write-card takes no arguments; name the disk with -device (flags come first)")
	}

	img, err := chooseImage(*image)
	if err != nil {
		return err
	}
	target, err := chooseDevice(out, *device)
	if err != nil {
		return err
	}
	if !*yes {
		ok, err := confirmWriteCard(out, os.Stdin, target, img)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("cancelled; nothing was written")
		}
	}

	out.logf("writing %s to %s", img.Path, card.RawPath(target.Path))
	report, err := card.Write(context.Background(), img.Path, target.Path, writeProgress(out, target.Path))
	if err != nil {
		return err
	}
	out.logf("wrote %s to %s in %s (%.1f MB/s)", humanBytes(report.Bytes), report.Device,
		report.Duration.Round(time.Second), report.Rate()/1e6)

	return out.result("write-card", struct {
		Device          string  `json:"device"`
		Image           string  `json:"image"`
		BuildID         string  `json:"build_id,omitempty"`
		Bytes           int64   `json:"bytes"`
		DurationSeconds float64 `json:"duration_seconds"`
	}{target.Path, img.Path, img.BuildID, report.Bytes, seconds(report.Duration)}, nil)
}

// The two images a card can be given. A golden card is a finished node; a
// vanilla one provisions itself on first boot and is what day 0 writes.
const (
	imageGolden  = "golden"
	imageVanilla = "vanilla"
)

// cardImage is the image write-card will write, and what is known about it.
type cardImage struct {
	Path    string
	BuildID string
}

func chooseImage(kind string) (cardImage, error) {
	switch kind {
	case imageGolden:
		img := cardImage{Path: goldenPath}
		if !fileExists(img.Path) {
			return img, fmt.Errorf("%s is missing: run `rasputin bake` first, or write the prepared image with -image vanilla", img.Path)
		}
		// The build id is provenance, not a precondition. A node with a dead
		// card is rescued with the bytes that are on disk, whatever out/meta
		// does or does not remember about them.
		if meta, err := cluster.ReadGoldenMetaAt(goldenMetaPath); err == nil {
			img.BuildID = meta.BuildID
		}
		return img, nil
	case imageVanilla:
		img := cardImage{Path: vanillaPath}
		if !fileExists(img.Path) {
			return img, fmt.Errorf("%s is missing: run `rasputin prepare` first", img.Path)
		}
		return img, nil
	}
	return cardImage{}, fmt.Errorf("-image is %q; it is %s or %s", kind, imageGolden, imageVanilla)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// cardTarget is the disk to write and what the operator was told about it.
type cardTarget struct {
	Path string
	// Detail is the size and model, when the disk came from the picker. A
	// disk named with -device is whatever the operator says it is.
	Detail string
}

// chooseDevice resolves -device, or asks. There is no default and no guess:
// the wrong answer erases the machine this runs on.
func chooseDevice(out *output, device string) (cardTarget, error) {
	if device != "" {
		return cardTarget{Path: device}, nil
	}
	switch {
	case out.json:
		return cardTarget{}, errors.New("-json cannot ask which disk to erase: name it with -device")
	case !tui.IsTerminal():
		return cardTarget{}, errors.New("there is no terminal to choose a disk on: name it with -device")
	}
	devices, err := card.List()
	if err != nil {
		return cardTarget{}, err
	}
	if len(devices) == 0 {
		return cardTarget{}, errors.New("no removable disk found: insert the card, or name the disk with -device")
	}
	rows := make([]tui.Device, len(devices))
	for i, d := range devices {
		rows[i] = tui.Device{Path: d.Path, Size: d.Size, Name: d.Name, Removable: d.Removable}
	}
	chosen, err := tui.PickDevice(context.Background(), rows)
	if err != nil {
		if errors.Is(err, tui.ErrNoDevice) {
			return cardTarget{}, errors.New("cancelled; nothing was written")
		}
		return cardTarget{}, err
	}
	return cardTarget{Path: chosen.Path, Detail: fmt.Sprintf("%s %s", humanBytes(chosen.Size), chosen.Name)}, nil
}

// confirmWriteCard shows what is about to happen to which disk and asks. The
// question names the disk because this is the one command whose mistake is
// made on the operator's own machine, where there is no recovery agent to
// catch it.
func confirmWriteCard(out *output, in *os.File, target cardTarget, img cardImage) (bool, error) {
	if out.json {
		return false, fmt.Errorf("writing %s erases everything on it and -json never prompts: re-run with -yes", target.Path)
	}
	if in == nil || !term.IsTerminal(int(in.Fd())) {
		return false, fmt.Errorf("writing %s erases everything on it and there is no terminal to confirm on: re-run with -yes", target.Path)
	}
	out.printf("image:  %s%s\n", img.Path, parenthesised("build "+img.BuildID, img.BuildID != ""))
	out.printf("device: %s%s\n", target.Path, parenthesised(target.Detail, target.Detail != ""))
	out.printf("This ERASES everything on %s.\n", target.Path)
	return readYesNo(out, in)
}

func parenthesised(s string, when bool) string {
	if !when {
		return ""
	}
	return " (" + s + ")"
}

// writeProgress reports the transfer: a line a second for a person, the same
// transfer events `sync` emits for a program.
func writeProgress(out *output, device string) func(written, total int64) {
	start := time.Now()
	return func(written, total int64) {
		rate := float64(written) / time.Since(start).Seconds()
		if out.json {
			out.emit(toJSONEvent(events.Event{
				Kind: events.Transfer, Step: writeCardStep, Node: device,
				Bytes: written, Total: total, Rate: rate,
			}))
			return
		}
		out.printf("written %s / %s (%.1f MB/s)\n", humanBytes(written), humanBytes(total), rate/1e6)
	}
}

// humanBytes renders a byte count the way an operator reads a card.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MiB", n>>20)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
