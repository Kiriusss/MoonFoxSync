//go:build windows

package engine

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func codexReparse(info os.FileInfo) bool {
	attr, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return !ok || attr.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.Mode()&os.ModeSymlink != 0
}
func openCodexFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

var codexReopen = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

func codexSignature(f *os.File) (codexSample, bool, error) {
	handle := windows.Handle(f.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return codexSample{}, false, err
	}
	if info.NumberOfLinks != 1 || info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return codexSample{}, false, nil
	}
	// Windows may defer timestamps while any write handle is open. Probe the
	// existing file identity without keeping a restrictive sharing mode during IO.
	probe, _, _ := codexReopen.Call(uintptr(handle), windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, 0)
	volatile := windows.Handle(probe) == windows.InvalidHandle
	if !volatile {
		defer windows.CloseHandle(windows.Handle(probe))
	}
	// FILE_BASIC_INFO supplies the metadata change clock in addition to LastWriteTime.
	var basic struct {
		Creation, Access, Write, Change int64
		Attrs, Padding                  uint32
	}
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return codexSample{}, false, err
	}
	size := int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
	// ReFS has 128-bit file IDs. On filesystems lacking FILE_ID_INFO avoid using
	// metadata as a cache key; still capture and verify the sampled prefix.
	var id struct {
		Volume uint64
		ID     [16]byte
	}
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id))); err != nil {
		volatile = true
	}
	key := fmt.Sprintf("%x:%x:%x:%x:%x", id.Volume, id.ID, size, basic.Write, basic.Change)
	return codexSample{size, key, volatile}, true, nil
}
