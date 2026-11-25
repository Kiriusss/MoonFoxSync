//go:build darwin

package engine

import (
	"golang.org/x/sys/unix"
	"net"
	"syscall"
)

func interfaceControl(i *net.Interface) func(string, string, syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var result error
		e := c.Control(func(fd uintptr) { result = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, i.Index) })
		if e != nil {
			return e
		}
		return result
	}
}
