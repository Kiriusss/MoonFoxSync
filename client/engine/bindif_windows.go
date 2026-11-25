//go:build windows

package engine

import (
	"golang.org/x/sys/windows"
	"math/bits"
	"net"
	"syscall"
)

// IP_UNICAST_IF takes the IPv4 interface index in network byte order on Windows.
func interfaceControl(i *net.Interface) func(string, string, syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var result error
		err := c.Control(func(fd uintptr) {
			result = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, 31, int(bits.ReverseBytes32(uint32(i.Index))))
		})
		if err != nil {
			return err
		}
		return result
	}
}
