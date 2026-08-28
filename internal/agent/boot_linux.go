//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ericovis/rasputin/internal/kmsg"
)

// Fixed device paths. A Pi 3 booting from SD always presents the card as
// mmcblk0 with p1 = FAT boot, p2 = ext4 root (FACTS.md).
const (
	Disk      = "/dev/mmcblk0"
	BootPart  = "/dev/mmcblk0p1"
	RootPart  = "/dev/mmcblk0p2"
	BootMount = "/boot"
	NewRoot   = "/newroot"
)

// DeviceWait is how long to wait for the SD card partitions to appear.
const (
	DeviceWait     = 10 * time.Second
	devicePollTick = 200 * time.Millisecond
)

// MountPseudoFS mounts devtmpfs, proc and sysfs, which everything else needs:
// /dev/kmsg for logging, /sys for the eth0 MAC and carrier state, /proc for
// uptime. Already-mounted filesystems are not an error.
func MountPseudoFS() error {
	for _, m := range []struct{ source, target, fstype string }{
		{"devtmpfs", "/dev", "devtmpfs"},
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
	} {
		if err := os.MkdirAll(m.target, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("mkdir %s: %w", m.target, err)
		}
		err := syscall.Mount(m.source, m.target, m.fstype, 0, "")
		if err != nil && err != syscall.EBUSY {
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
		}
	}
	return nil
}

// WaitForDisk blocks until both SD card partitions exist, or timeout elapses.
func WaitForDisk(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err1 := os.Stat(BootPart)
		_, err2 := os.Stat(RootPart)
		if err1 == nil && err2 == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("SD card partitions %s and %s never appeared after %s", BootPart, RootPart, timeout)
		}
		time.Sleep(devicePollTick)
	}
}

// ReadFlags mounts the boot partition read-only, reads whichever flag files
// exist, and unmounts again. Reading read-only means a half-written FAT can
// never be made worse by the recovery path.
func ReadFlags() (map[string]string, error) {
	if err := os.MkdirAll(BootMount, 0o755); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if err := syscall.Mount(BootPart, BootMount, "vfat", syscall.MS_RDONLY, ""); err != nil {
		return nil, fmt.Errorf("mount %s ro: %w", BootPart, err)
	}
	defer func() { _ = syscall.Unmount(BootMount, 0) }()

	files := map[string]string{}
	for _, name := range []string{FlagDryrun, FlagCapture, FlagReflash} {
		data, err := os.ReadFile(filepath.Join(BootMount, name))
		if err != nil {
			continue // absent (or unreadable, which we treat the same way)
		}
		files[name] = string(data)
	}
	return files, nil
}

// WithBootRW mounts the boot partition read-write, runs fn against its mount
// point, syncs and unmounts. Used to drop a dryrun report and to clear flags.
func WithBootRW(fn func(dir string) error) error {
	if err := os.MkdirAll(BootMount, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	if err := syscall.Mount(BootPart, BootMount, "vfat", 0, ""); err != nil {
		return fmt.Errorf("mount %s rw: %w", BootPart, err)
	}
	err := fn(BootMount)
	syscall.Sync()
	if uerr := syscall.Unmount(BootMount, 0); uerr != nil && err == nil {
		err = fmt.Errorf("unmount %s: %w", BootMount, uerr)
	}
	return err
}

// SwitchRoot hands control to the real system, exactly as busybox
// switch_root does: mount p2 read-only (systemd fsck's and remounts it rw
// itself), move /dev across, then move-mount the new root over / and exec
// its init. It only returns on failure — on success this process is replaced.
func SwitchRoot(log *kmsg.Logger) error {
	if err := os.MkdirAll(NewRoot, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	var mountErr error
	for attempt := 1; attempt <= 3; attempt++ {
		// ext4 read-only: the real init is responsible for fsck + remount.
		mountErr = syscall.Mount(RootPart, NewRoot, "ext4", syscall.MS_RDONLY, "")
		if mountErr == nil {
			break
		}
		log.Printf("mount %s ro attempt %d failed: %v", RootPart, attempt, mountErr)
		time.Sleep(time.Second)
	}
	if mountErr != nil {
		return fmt.Errorf("mount rootfs %s: %w", RootPart, mountErr)
	}

	// /proc and /sys are recreated by the real init; /dev must survive the
	// pivot or the new init has no console.
	_ = syscall.Unmount("/sys", 0)
	_ = syscall.Unmount("/proc", 0)
	if err := os.MkdirAll(filepath.Join(NewRoot, "dev"), 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	if err := syscall.Mount("/dev", filepath.Join(NewRoot, "dev"), "", syscall.MS_MOVE, ""); err != nil {
		log.Printf("moving /dev into the new root failed (%v); unmounting instead", err)
		_ = syscall.Unmount("/dev", 0)
	}

	if err := syscall.Chdir(NewRoot); err != nil {
		return fmt.Errorf("chdir %s: %w", NewRoot, err)
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move-mount new root over /: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	const realInit = "/sbin/init"
	return syscall.Exec(realInit, []string{realInit}, os.Environ())
}

// Reboot forces an immediate restart. It never returns on success.
func Reboot() error {
	syscall.Sync()
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}

// MAC returns eth0's hardware address, or "unknown". Every HTTP call carries
// it so the CLI can tell which node is talking to it.
func MAC() string {
	data, err := os.ReadFile("/sys/class/net/eth0/address")
	if err != nil {
		return "unknown"
	}
	return FirstLine(string(data))
}
