package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type directoryCheck struct {
	path string
	sum  [sha256.Size]byte
}

// Keep one digest per directory for the end-of-capture race check. On Unix
// directory entries carry both device and inode. Windows retains its native
// SameFile check because identity there must be obtained from a file handle.
func hashStat(h hash.Hash, name string, info fs.FileInfo) {
	id, _ := workspaceGitIdentity(info)
	var raw [56]byte
	for i, v := range [7]uint64{uint64(len(name)), id[0], id[1], uint64(info.Mode()), uint64(info.Size()), uint64(info.ModTime().UnixNano()), uint64(fileChangeTime(info))} {
		binary.LittleEndian.PutUint64(raw[i*8:], v)
	}
	h.Write(raw[:])
	h.Write([]byte(name))
}

func (c *capture) walkChildren(path string, dir *os.Root, expected fs.FileInfo, visit func(string, string, fs.FileInfo) error) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := dir.Open(".")
	if err != nil {
		return sum, err
	}
	info, err := f.Stat()
	if err != nil || expected != nil && !sameFile(expected, info) {
		f.Close()
		return sum, fmt.Errorf("采集期间目录已变化：%s", path)
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return sum, err
	}
	sort.Strings(names)
	h := sha256.New()
	if runtime.GOOS != "windows" {
		hashStat(h, ".", info)
	}
	for _, name := range names {
		if err := c.ctx.Err(); err != nil {
			return sum, err
		}
		if strings.EqualFold(name, ".git") {
			continue
		}
		child := filepath.Join(path, name)
		if c.excluded[child] || (c.included != nil && !c.included[child]) {
			continue
		}
		st, err := dir.Lstat(name)
		if err != nil {
			return sum, err
		}
		if st.IsDir() {
			if _, err := dir.Lstat(filepath.Join(name, ".git")); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return sum, err
			}
		}
		if runtime.GOOS != "windows" {
			hashStat(h, name, st)
		}
		if visit != nil {
			if err := visit(name, child, st); err != nil {
				return sum, err
			}
		}
	}
	return [sha256.Size]byte(h.Sum(sum[:0])), nil
}

func (c *capture) verifyDirectories() error {
	for _, check := range c.directoryChecks {
		dir, err := c.root.OpenRoot(check.path)
		if err != nil {
			return err
		}
		sum, err := c.walkChildren(check.path, dir, nil, nil)
		dir.Close()
		if err != nil {
			return err
		}
		if sum != check.sum {
			return fmt.Errorf("采集期间目录或文件已变动，请重试: %s", check.path)
		}
	}
	return nil
}
