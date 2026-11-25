//go:build darwin || linux

package engine

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func codexReparse(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }
func openCodexFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
func codexSignature(f *os.File) (codexSample, bool, error) {
	var st unix.Stat_t
	err := unix.Fstat(int(f.Fd()), &st)
	if err != nil {
		return codexSample{}, false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return codexSample{}, false, nil
	}
	return codexSample{st.Size, fmt.Sprintf("%x:%x:%d:%d:%d:%d:%d", st.Dev, st.Ino, st.Size, st.Mtim.Sec, st.Mtim.Nsec, st.Ctim.Sec, st.Ctim.Nsec), false}, true, nil
}
