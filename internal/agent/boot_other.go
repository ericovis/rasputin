//go:build !linux

package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"time"

	"github.com/ericovis/rasputin/internal/kmsg"
)

// These stubs exist only so the module builds, vets and tests on the darwin
// build host. The agent itself is always cross-compiled for linux/arm64; on
// any other GOOS every entry point below fails loudly instead of pretending.

const (
	DiskPath  = "/dev/mmcblk0"
	BootPart  = "/dev/mmcblk0p1"
	RootPart  = "/dev/mmcblk0p2"
	BootMount = "/boot"
	NewRoot   = "/newroot"
)

const DeviceWait = 10 * time.Second

func unsupported(op string) error {
	return fmt.Errorf("agent: %s is linux-only (running on %s)", op, runtime.GOOS)
}

func MountPseudoFS() error                  { return unsupported("mounting pseudo filesystems") }
func WaitForDisk(time.Duration) error       { return unsupported("waiting for the SD card") }
func ReadFlags() (map[string]string, error) { return nil, unsupported("reading boot flags") }
func WithBootRW(func(string) error) error   { return unsupported("writing to the boot partition") }
func SwitchRoot(*kmsg.Logger) error         { return unsupported("switch_root") }
func Reboot() error                         { return unsupported("reboot") }
func MAC() string                           { return "unknown" }

// SDCard and Sys have no meaning off Linux; they exist so cmd/agent compiles
// on the build host.
func SDCard() Disk { return unsupportedDisk{} }
func Sys() System  { return unsupportedSystem{} }

type unsupportedDisk struct{}

func (unsupportedDisk) OpenWrite() (Target, error) { return nil, unsupported("opening the SD card") }
func (unsupportedDisk) OpenRead() (io.ReadCloser, int64, error) {
	return nil, 0, unsupported("reading the SD card")
}
func (unsupportedDisk) Close() error { return nil }

type unsupportedSystem struct{}

func (unsupportedSystem) WithBoot(func(string) error) error { return unsupported("the boot partition") }
func (unsupportedSystem) Reboot() error                     { return unsupported("reboot") }

// NetworkUp is the non-Linux stub of the DHCP bring-up.
func NetworkUp(context.Context, *kmsg.Logger) (net.IP, error) {
	return nil, unsupported("bringing up the network")
}
