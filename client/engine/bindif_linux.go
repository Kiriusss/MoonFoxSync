//go:build linux

package engine

import (
	"golang.org/x/sys/unix"
	"net"
	"syscall"
)

func interfaceControl(i *net.Interface) func(string, string, syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var result error
		e := c.Control(func(fd uintptr) {
			result = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, i.Name)
		})
		if e != nil {
			return e
		}
		return result
	}
}
