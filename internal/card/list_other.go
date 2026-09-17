//go:build !darwin

package card

import "errors"

// The build host this tool is driven from is a Mac, and finding the SD card
// reader is the only part of write-card that is not portable. Everywhere else
// the disk has to be named: writing it is the same code, and naming the wrong
// disk is the same mistake, so the one thing not offered is a guess.

// List reports that there is nothing to pick from here.
func List() ([]Device, error) {
	return nil, errors.New("finding SD card readers is implemented on macOS only: name the disk with -device")
}

// unmount is macOS's diskutil dance; elsewhere the operator owns it.
func unmount(string) error { return nil }

func eject(string) {}
