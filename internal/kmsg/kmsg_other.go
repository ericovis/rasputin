//go:build !linux

package kmsg

import (
	"fmt"
	"os"
	"runtime"
)

// Open is the non-Linux stub: there is no kernel ring buffer to write to, so
// it degrades to stdout. It exists so the package builds (and the CLI's
// tooling links) on the darwin build host.
func Open() (*Logger, *os.File, error) {
	return New(os.Stdout), nil, fmt.Errorf("kmsg: /dev/kmsg unavailable on %s", runtime.GOOS)
}
