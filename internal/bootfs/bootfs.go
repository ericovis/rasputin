// Package bootfs edits the FAT32 boot partition of a Raspberry Pi OS image.
//
// Only the boot partition is ever touched. The ext4 root partition is left
// exactly as the Raspberry Pi Foundation shipped it: everything we want to
// change inside the root filesystem is done on the Pi itself, by the
// firstrun.sh script this package installs. That keeps image preparation to
// pure-Go FAT writes with no loop mounts, no root, and no Docker.
package bootfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
)

// BootPartition is the 1-based index of the FAT boot partition in a stock
// Raspberry Pi OS image.
const BootPartition = 1

// Image is an open Raspberry Pi OS image, positioned on its boot partition.
type Image struct {
	path string
	disk *disk.Disk
	fs   filesystem.FileSystem
}

// Open opens an image file for reading and writing its boot partition.
func Open(path string) (*Image, error) {
	d, err := diskfs.Open(path, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	fsys, err := d.GetFilesystem(BootPartition)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("opening the boot partition of %s: %w", path, err)
	}
	if fsys.Type() != filesystem.TypeFat32 {
		d.Close()
		return nil, fmt.Errorf("partition %d of %s is not FAT32", BootPartition, path)
	}
	return &Image{path: path, disk: d, fs: fsys}, nil
}

// Close flushes and releases the image.
func (img *Image) Close() error {
	var errs []error
	if img.fs != nil {
		if err := img.fs.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if img.disk != nil {
		if err := img.disk.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	img.fs, img.disk = nil, nil
	return errors.Join(errs...)
}

// Path is the image file this was opened from.
func (img *Image) Path() string { return img.path }

// normalize converts a caller's name into the io/fs form the go-diskfs FAT
// driver validates against: unrooted, with "." for the partition root.
func normalize(name string) string {
	name = strings.Trim(name, "/")
	if name == "" {
		return "."
	}
	return name
}

// ReadFile reads a file from the boot partition.
//
// The read is bounded by the size in the directory entry rather than by EOF:
// the go-diskfs FAT driver keeps handing back the tail of the last cluster
// after the file has ended (stock config.txt is 1272 bytes but reads back as
// 1536), which would otherwise smuggle NUL padding into files we rewrite.
func (img *Image) ReadFile(name string) ([]byte, error) {
	p := normalize(name)
	info, err := img.fs.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("reading %s from the boot partition: %w", name, err)
	}
	f, err := img.fs.OpenFile(p, os.O_RDONLY)
	if err != nil {
		return nil, fmt.Errorf("reading %s from the boot partition: %w", name, err)
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, fmt.Errorf("reading %s from the boot partition: %w", name, err)
	}
	return data, nil
}

// WriteFile creates or replaces a file on the boot partition.
//
// The existing file is removed first rather than truncated: FAT32 writes
// through this library extend a file's cluster chain, so overwriting a
// shorter file in place would leave the tail of the old content behind.
func (img *Image) WriteFile(name string, data []byte) error {
	p := normalize(name)
	if _, err := img.fs.Stat(p); err == nil {
		if err := img.fs.Remove(p); err != nil {
			return fmt.Errorf("replacing %s on the boot partition: %w", name, err)
		}
	}
	f, err := img.fs.OpenFile(p, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("creating %s on the boot partition: %w", name, err)
	}
	n, err := f.Write(data)
	if err != nil {
		return fmt.Errorf("writing %s on the boot partition: %w", name, err)
	}
	if n != len(data) {
		return fmt.Errorf("short write of %s: %d of %d bytes", name, n, len(data))
	}
	return nil
}

// Remove deletes a file from the boot partition. An absent file is success.
func (img *Image) Remove(name string) error {
	p := normalize(name)
	if _, err := img.fs.Stat(p); err != nil {
		return nil
	}
	if err := img.fs.Remove(p); err != nil {
		return fmt.Errorf("removing %s from the boot partition: %w", name, err)
	}
	return nil
}

// Exists reports whether a file is present on the boot partition.
func (img *Image) Exists(name string) bool {
	_, err := img.fs.Stat(normalize(name))
	return err == nil
}

// List returns the sorted names of the files in the root of the boot
// partition.
func (img *Image) List() ([]string, error) {
	entries, err := img.fs.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("listing the boot partition: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Stat returns metadata for one file on the boot partition.
func (img *Image) Stat(name string) (fs.FileInfo, error) {
	return img.fs.Stat(normalize(name))
}
