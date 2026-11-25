//go:build linux

package engine

import "golang.org/x/sys/unix"

func publishNew(src, dst string) error {
	return unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
}
