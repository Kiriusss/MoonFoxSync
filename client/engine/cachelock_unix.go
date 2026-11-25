//go:build darwin || linux

package engine

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func cacheLock(path string) (*os.File, error) {
	return lockFile(path, true)
}

func lockFile(path string, exclusive bool) (*os.File, error) {
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	f, e := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if e != nil {
		return nil, e
	}
	if e = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}

func lockBusy(err error) bool { return errors.Is(err, unix.EWOULDBLOCK) }
