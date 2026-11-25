//go:build windows

package engine

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func directoryMarker(f *os.File) (string, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

func workspaceGitIdentity(info os.FileInfo) ([2]uint64, bool) {
	return [2]uint64{}, false
}
