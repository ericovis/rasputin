//go:build linux

package agent

import (
	"fmt"
	"io"
	"os"

	"github.com/ericovis/rasputin/internal/mbr"
)

// sdCard is the production Disk: the whole SD card as a block device.
type sdCard struct {
	path string
	f    *os.File
}

// SDCard returns a Disk backed by /dev/mmcblk0.
func SDCard() Disk { return &sdCard{path: DiskPath} }

// OpenWrite opens the raw block device. Writing here destroys the running
// system's card, which is safe only because the agent lives in RAM.
func (d *sdCard) OpenWrite() (Target, error) {
	f, err := os.OpenFile(d.path, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	d.f = f
	return syncRangeTarget{f}, nil
}

// OpenRead opens the card for reading and reports how much of it is in use,
// so a capture streams a few GB rather than the whole 32 GB card.
func (d *sdCard) OpenRead() (io.ReadCloser, int64, error) {
	f, err := os.Open(d.path)
	if err != nil {
		return nil, 0, err
	}
	used, err := mbr.UsedBytes(f)
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("reading the partition table of %s: %w", d.path, err)
	}
	d.f = f
	return f, used, nil
}

func (d *sdCard) Close() error {
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

// linuxSystem is the production System.
type linuxSystem struct{}

// Sys returns the production System: the real boot partition and a real
// reboot.
func Sys() System { return linuxSystem{} }

func (linuxSystem) WithBoot(fn func(dir string) error) error { return WithBootRW(fn) }
func (linuxSystem) Reboot() error                            { return Reboot() }
