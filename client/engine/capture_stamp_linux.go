package engine

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func fileChangeTime(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Nano()
	}
	return 0
}

func captureVolume(f *os.File) (uint64, bool) {
	var fs unix.Statfs_t
	var st unix.Stat_t
	if unix.Fstatfs(int(f.Fd()), &fs) != nil || unix.Fstat(int(f.Fd()), &st) != nil {
		return 0, false
	}
	switch uint64(fs.Type) {
	case unix.EXT4_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.TMPFS_MAGIC:
		return uint64(st.Dev), true
	}
	return 0, false
}
