// Package cpio writes the "newc" (SVR4, no CRC) archive format used by the
// Linux kernel for initramfs images.
//
// It exists because the initramfs must contain a /dev/console character device
// and mknod needs privileges we do not have (and does not exist on darwin, the
// build host). Emitting the entry bytes directly sidesteps both problems.
package cpio

import (
	"fmt"
	"io"
	"os"
)

// Header field constants. See FACTS.md; every numeric field is 8 ASCII hex
// digits, and both the name and the data are padded to a 4-byte boundary.
const (
	magic      = "070701"
	headerSize = 110
	trailer    = "TRAILER!!!"

	// modeFmt masks off the file-type bits of a mode field.
	modeFmt  = 0o170000
	modeDir  = 0o040000
	modeFile = 0o100000
	modeChar = 0o020000
)

// Writer emits newc entries to an underlying stream.
type Writer struct {
	w      io.Writer
	ino    uint32
	closed bool
	err    error
}

// NewWriter returns a Writer that appends entries to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteDir adds a directory entry. name is archive-relative ("dev", not "/dev").
func (w *Writer) WriteDir(name string) error {
	return w.entry(name, modeDir|0o755, 2, 0, 0, nil)
}

// WriteFile adds a regular file with the given permission bits and contents.
func (w *Writer) WriteFile(name string, mode os.FileMode, data []byte) error {
	return w.entry(name, modeFile|uint32(mode.Perm()), 1, 0, 0, data)
}

// WriteCharDev adds a character device node, e.g. dev/console as 5:1.
func (w *Writer) WriteCharDev(name string, mode os.FileMode, major, minor int) error {
	return w.entry(name, modeChar|uint32(mode.Perm()), 1, uint32(major), uint32(minor), nil)
}

// Close writes the TRAILER!!! entry. It does not close the underlying writer.
func (w *Writer) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return nil
	}
	w.closed = true
	// The trailer carries no ino of its own; conventionally every field but
	// nlink and namesize is zero.
	return w.write(header(trailer, 0, 0, 1, 0, 0, 0), pad(headerSize+len(trailer)+1))
}

func (w *Writer) entry(name string, mode uint32, nlink, rmaj, rmin uint32, data []byte) error {
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return fmt.Errorf("cpio: write after Close")
	}
	if name == "" || name[0] == '/' {
		return fmt.Errorf("cpio: name %q must be relative and non-empty", name)
	}
	w.ino++
	h := header(name, w.ino, mode, nlink, uint32(len(data)), rmaj, rmin)
	return w.write(h, pad(headerSize+len(name)+1), data, pad(len(data)))
}

func (w *Writer) write(chunks ...[]byte) error {
	for _, c := range chunks {
		if len(c) == 0 {
			continue
		}
		if _, err := w.w.Write(c); err != nil {
			w.err = err
			return err
		}
	}
	return nil
}

// header builds the 110-byte ASCII header plus the NUL-terminated name.
func header(name string, ino, mode, nlink, filesize, rmaj, rmin uint32) []byte {
	b := make([]byte, 0, headerSize+len(name)+1)
	b = append(b, magic...)
	for _, f := range []uint32{
		ino,                   // ino
		mode,                  // mode
		0,                     // uid
		0,                     // gid
		nlink,                 // nlink
		0,                     // mtime — zero keeps builds reproducible
		filesize,              // filesize
		0,                     // devmajor
		0,                     // devminor
		rmaj,                  // rdevmajor
		rmin,                  // rdevminor
		uint32(len(name)) + 1, // namesize, including the NUL
		0,                     // check — always zero for newc
	} {
		b = appendHex8(b, f)
	}
	b = append(b, name...)
	b = append(b, 0)
	return b
}

func appendHex8(b []byte, v uint32) []byte {
	const digits = "0123456789abcdef"
	for shift := 28; shift >= 0; shift -= 4 {
		b = append(b, digits[(v>>uint(shift))&0xf])
	}
	return b
}

// pad returns the NUL bytes needed to round n up to a multiple of 4.
func pad(n int) []byte {
	if r := n % 4; r != 0 {
		return make([]byte, 4-r)
	}
	return nil
}
