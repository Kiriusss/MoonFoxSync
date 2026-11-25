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

func workspaceGitIdentity(info os.FileInfo) ([2]uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]uint64{}, false
	}
	return [2]uint64{uint64(stat.Dev), uint64(stat.Ino)}, true
}
