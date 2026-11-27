package engine

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func fileChangeTime(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctimespec.Nano()
	}
	return 0
}

func captureVolume(f *os.File) (uint64, bool) {
	var fs unix.Statfs_t
	var st unix.Stat_t
	if unix.Fstatfs(int(f.Fd()), &fs) != nil || unix.Fstat(int(f.Fd()), &st) != nil {
		return 0, false
	}
	return uint64(st.Dev), unix.ByteSliceToString(fs.Fstypename[:]) == "apfs" && fs.Flags&unix.MNT_LOCAL != 0
}
