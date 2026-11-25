//go:build windows

package engine

import "golang.org/x/sys/windows"

func publishNew(src, dst string) error {
	a, e := windows.UTF16PtrFromString(src)
	if e != nil {
		return e
	}
	b, e := windows.UTF16PtrFromString(dst)
	if e != nil {
		return e
	}
	return windows.MoveFile(a, b)
}
