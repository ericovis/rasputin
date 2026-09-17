package card

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Discovery and mounting on macOS is diskutil's job. It is asked for plists,
// which are then converted to JSON by plutil rather than parsed here: both
// tools are in the base system, and this way the module needs no plist
// dependency for one command that only runs on the operator's Mac.

// diskutilTimeout bounds every call. diskutil talks to diskarbitrationd, and
// a card that is half-out of the reader can hang it for minutes.
const diskutilTimeout = 30 * time.Second

// List returns the external physical disks: card readers, USB sticks, and
// nothing that is part of this machine. diskutil is asked for `external
// physical` and the result is filtered again in device().
func List() ([]Device, error) {
	raw, err := plistJSON("diskutil", "list", "-plist", "external", "physical")
	if err != nil {
		return nil, err
	}
	ids, err := wholeDisks(raw)
	if err != nil {
		return nil, err
	}
	var out []Device
	for _, id := range ids {
		info, err := plistJSON("diskutil", "info", "-plist", id)
		if err != nil {
			// A card pulled out between the two calls is not an error; it is
			// one fewer disk to choose from.
			continue
		}
		if d, ok := device(info); ok {
			out = append(out, d)
		}
	}
	return out, nil
}

// unmount frees the disk so the raw node can be written. Without it macOS
// holds the partitions open and every write fails with "Resource busy".
func unmount(dev string) error {
	if _, err := run("diskutil", "unmountDisk", dev); err != nil {
		return err
	}
	return nil
}

// eject is best effort: the card is written and verified whether or not the
// operator has to unmount it themselves afterwards.
func eject(dev string) { run("diskutil", "eject", dev) }

// plistJSON runs a command that prints a plist and returns it as JSON.
func plistJSON(name string, args ...string) ([]byte, error) {
	plist, err := run(name, args...)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("plutil", "-convert", "json", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(plist)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("plutil: %v: %s", err, strings.TrimSpace(errs.String()))
	}
	return out.Bytes(), nil
}

func run(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errs.String()))
		}
	case <-time.After(diskutilTimeout):
		cmd.Process.Kill()
		<-done
		return nil, fmt.Errorf("%s %s: no answer after %s", name, strings.Join(args, " "), diskutilTimeout)
	}
	return out.Bytes(), nil
}
