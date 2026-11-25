//go:build darwin || linux

package server

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// openat anchors each component to a directory descriptor; symlinks never cross
// the registered boundary, even when a directory is renamed during a request.
const openFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK

func openRegisteredRoot(p string) (*os.File, error) {
	fd, e := unix.Open(p, openFlags|unix.O_DIRECTORY, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), p), nil
}
func stateFrom(st unix.Stat_t) nodeState {
	kind := ""
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFREG:
		kind = "file"
	case unix.S_IFDIR:
		kind = "directory"
	case unix.S_IFLNK:
		kind = "symlink"
	}
	return nodeState{fmt.Sprintf("%d:%d", st.Dev, st.Ino), st.Size, kind, time.Unix(st.Mtim.Unix()), time.Unix(st.Ctim.Unix())}
}
func fileState(f *os.File) (nodeState, error) {
	var st unix.Stat_t
	e := unix.Fstat(int(f.Fd()), &st)
	return stateFrom(st), e
}
func statChild(f *os.File, name string) (nodeState, error) {
	var st unix.Stat_t
	e := unix.Fstatat(int(f.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	return stateFrom(st), e
}
func externalOpenError(e error) error {
	if errors.Is(e, unix.ENOENT) || errors.Is(e, unix.ENOTDIR) || errors.Is(e, unix.ELOOP) || errors.Is(e, unix.EACCES) {
		return absent()
	}
	return e
}
func (x *externalRoot) open(rel string, dir bool) (*os.File, error) {
	if !validPath(rel) {
		return nil, bad("invalid relative path")
	}
	f, e := openRegisteredRoot(x.Path)
	if e != nil {
		return nil, externalOpenError(e)
	}
	initial, e := fileState(f)
	if e != nil || initial.Identity != x.Identity {
		f.Close()
		return nil, conflict("registered source identity has changed")
	}
	if rel == "" {
		return f, nil
	}
	parts := strings.Split(rel, "/")
	for i, name := range parts {
		flags := openFlags
		if i < len(parts)-1 || dir {
			flags |= unix.O_DIRECTORY
		}
		if i == len(parts)-1 && !dir {
			st, e := statChild(f, name)
			if e != nil || st.Kind != "file" {
				f.Close()
				if e != nil {
					return nil, externalOpenError(e)
				}
				return nil, absent()
			}
		}
		fd, err := unix.Openat(int(f.Fd()), name, flags, 0)
		f.Close()
		if err != nil {
			return nil, externalOpenError(err)
		}
		f = os.NewFile(uintptr(fd), name)
	}
	return f, nil
}
