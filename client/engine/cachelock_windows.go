//go:build windows

package engine

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func cacheLock(path string) (*os.File, error) {
	return lockFile(path, true)
}

func lockFile(path string, exclusive bool) (*os.File, error) {
	mode := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		mode |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	f, e := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if e != nil {
		return nil, e
	}
	if e = windows.LockFileEx(windows.Handle(f.Fd()), mode, 0, 1, 0, &windows.Overlapped{}); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}

func lockBusy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }
