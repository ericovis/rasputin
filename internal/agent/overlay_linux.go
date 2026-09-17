//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/ericovis/rasputin/internal/kmsg"
)

// WithUpperRW mounts the writable layer read-write, runs fn against its mount
// point, syncs and unmounts. This is how a reset gets at the upper layer
// before anything has been built on top of it.
func WithUpperRW(fn func(dir string) error) error {
	if err := os.MkdirAll(UpperMount, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	if err := syscall.Mount(UpperPart, UpperMount, "ext4", syscall.MS_NOATIME, ""); err != nil {
		return fmt.Errorf("mount %s rw: %w", UpperPart, err)
	}
	err := fn(UpperMount)
	syscall.Sync()
	if uerr := syscall.Unmount(UpperMount, 0); uerr != nil && err == nil {
		err = fmt.Errorf("unmount %s: %w", UpperMount, uerr)
	}
	return err
}

// mountOverlayRoot assembles the real root at NewRoot out of the two layers:
// the golden rootfs read-only underneath, the writable layer on top. On any
// failure it leaves the caller to fall back to the plain read-only rootfs —
// see SwitchRoot.
func mountOverlayRoot(log *kmsg.Logger) error {
	for _, dir := range []string{LowerMount, UpperMount, NewRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	// Read-only, and it stays that way: the lower layer of an overlay must
	// not change underneath it, and nothing in the running system has a
	// reason to.
	if err := syscall.Mount(RootPart, LowerMount, "ext4", syscall.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount %s ro on %s: %w", RootPart, LowerMount, err)
	}
	if err := syscall.Mount(UpperPart, UpperMount, "ext4", syscall.MS_NOATIME, ""); err != nil {
		return fmt.Errorf("mount %s rw on %s: %w", UpperPart, UpperMount, err)
	}
	upper := filepath.Join(UpperMount, UpperSubdir)
	for _, dir := range []string{upper, filepath.Join(UpperMount, WorkSubdir)} {
		if err := os.MkdirAll(dir, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	release, err := kernelRelease()
	if err != nil {
		return err
	}
	if err := EnsureOverlayModule(LowerMount, upper, release, log); err != nil {
		return err
	}
	if err := syscall.Mount("overlay", NewRoot, "overlay", 0, OverlayOptions(LowerMount, UpperMount)); err != nil {
		return fmt.Errorf("mount the overlay on %s: %w", NewRoot, err)
	}
	moveLayersInside(log)
	return nil
}

// moveLayersInside re-mounts the two layers under LayersDir in the new root,
// so an operator on the node can see and measure them. It is best effort: the
// mount points only exist in a golden image built after firstrun learned to
// create them, and a node that boots is worth more than a tidy `df`.
func moveLayersInside(log *kmsg.Logger) {
	for _, m := range []struct{ from, sub string }{
		{LowerMount, "lower"},
		{UpperMount, "upper"},
	} {
		target := filepath.Join(NewRoot, LayersDir, m.sub)
		if _, err := os.Stat(target); err != nil {
			logf(log, "overlay: %s does not exist in the rootfs; leaving %s where it is", target, m.from)
			continue
		}
		if err := syscall.Mount(m.from, target, "", syscall.MS_MOVE, ""); err != nil {
			logf(log, "overlay: could not move %s to %s: %v", m.from, target, err)
		}
	}
}

// unmountLayers undoes whatever mountOverlayRoot managed before it failed, so
// the fallback finds NewRoot free. Outermost first.
func unmountLayers() {
	for _, dir := range []string{NewRoot, UpperMount, LowerMount} {
		_ = syscall.Unmount(dir, 0)
	}
}

// kernelRelease is `uname -r`: the directory under /lib/modules holding the
// modules this kernel will accept.
func kernelRelease() (string, error) {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "", fmt.Errorf("uname: %w", err)
	}
	release := make([]byte, 0, len(u.Release))
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		release = append(release, byte(c))
	}
	return string(release), nil
}

// initModule loads a decompressed kernel module image. There is no userspace
// module loader in the initramfs, and none is needed: the image is complete
// and overlay depends on nothing.
func initModule(image []byte) error {
	if len(image) == 0 {
		return fmt.Errorf("the overlay module image is empty")
	}
	params := []byte{0} // init_module(2) wants a NUL-terminated parameter string
	_, _, errno := syscall.Syscall(syscall.SYS_INIT_MODULE,
		uintptr(unsafe.Pointer(&image[0])), uintptr(len(image)),
		uintptr(unsafe.Pointer(&params[0])))
	// EEXIST means someone else loaded it first, which is the state we want.
	if errno != 0 && errno != syscall.EEXIST {
		return fmt.Errorf("init_module: %w", errno)
	}
	return nil
}
