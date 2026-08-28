//go:build linux

package kmsg

import (
	"os"
)

// Open returns a Logger writing to both /dev/kmsg and stdout. If /dev/kmsg
// cannot be opened (it needs devtmpfs mounted first), the returned Logger
// still works, writing to stdout only, and the error explains why.
//
// The caller owns the returned io.Closer; the agent keeps it open for its
// whole life, so it normally discards it.
func Open() (*Logger, *os.File, error) {
	f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0)
	if err != nil {
		return New(os.Stdout), nil, err
	}
	return New(f, os.Stdout), f, nil
}
