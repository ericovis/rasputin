//go:build linux

package agent

import (
	"os"
	"syscall"
)

// sync_file_range(2) flags, from include/uapi/linux/fs.h. The stdlib
// syscall package has the linux/arm64 SyncFileRange wrapper but not these
// constants.
const (
	syncFileRangeWaitBefore = 0x1
	syncFileRangeWrite      = 0x2
	syncFileRangeWaitAfter  = 0x4
)

// syncRangeTarget wraps the raw SD block device with pipelined writeback
// control, so copyWithProgress can flush range N in the background while
// range N+1 streams in. Sync is inherited from *os.File unchanged: the
// final full fsync in Stream remains the durability barrier before reboot,
// because sync_file_range never flushes the device cache.
type syncRangeTarget struct{ *os.File }

var (
	_ Target      = syncRangeTarget{}
	_ RangeSyncer = syncRangeTarget{}
)

func (t syncRangeTarget) StartWriteback(off, n int64) error {
	return syscall.SyncFileRange(int(t.Fd()), off, n, syncFileRangeWrite)
}

func (t syncRangeTarget) AwaitWriteback(off, n int64) error {
	return syscall.SyncFileRange(int(t.Fd()), off, n,
		syncFileRangeWaitBefore|syncFileRangeWrite|syncFileRangeWaitAfter)
}
