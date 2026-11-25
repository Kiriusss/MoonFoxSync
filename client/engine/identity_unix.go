//go:build darwin || linux

package engine

import (
	"fmt"
	"os"
	"syscall"
)

func directoryMarker(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return "", fmt.Errorf("无法确认目录身份")
	}
	return fmt.Sprintf("%x:%x", stat.Dev, stat.Ino), nil
}
