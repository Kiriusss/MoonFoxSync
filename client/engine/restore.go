package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type entry struct {
	Path       string     `json:"path"`
	Kind       string     `json:"kind"`
	Size       *int64     `json:"size"`
	Mtime      *time.Time `json:"mtime"`
	Executable bool       `json:"executable"`
	Target     string     `json:"target"`
}

func localSegment(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) && !strings.ContainsRune(name, '\x00') && !strings.Contains(name, "/") && filepath.Base(name) == name && filepath.IsLocal(name) && !strings.EqualFold(name, ".git")
}
func remotePathOK(s string) bool {
	if s == "" {
		return true
	}
	for _, part := range strings.Split(s, "/") {
		if !localSegment(part) {
			return false
		}
	}
	return true
}

type RestoreResult struct {
	Path        string `json:"path"`
	Files       int    `json:"files"`
	Directories int    `json:"directories"`
	Bytes       int64  `json:"bytes"`
	Snapshot    string `json:"snapshotId"`
}

func (r *Remote) Restore(ctx context.Context, unit, snapshot, member, destination string) (RestoreResult, error) {
	var res RestoreResult
	if e := validID(unit); e != nil {
		return res, e
	}
	if e := validID(snapshot); e != nil {
		return res, e
	}
	if !remotePathOK(member) || destination == "" {
		return res, errors.New("无效的成员或目标路径")
	}
	dst, e := filepath.Abs(destination)
	if e != nil {
		return res, e
	}
	if _, e = os.Lstat(dst); !os.IsNotExist(e) {
		if e == nil {
			e = errors.New("恢复目标必须是不存在的新目录")
		}
		return res, e
	}
	tmp, e := os.MkdirTemp(filepath.Dir(dst), ".xinhu-restore-")
	if e != nil {
		return res, e
	}
	defer os.RemoveAll(tmp)
	res.Path = dst
	res.Snapshot = snapshot
	progress := startTransfer(r.stateHome, "download", unit, snapshot)
	defer progress.close()
	var dir func(string, string, int) error
	var materialize func(entry, string, int) error
	materialize = func(item entry, local string, depth int) error {
		var err error
		switch item.Kind {
		case "directory":
			if err = os.Mkdir(local, 0700); err == nil {
				res.Directories++
				err = dir(item.Path, local, depth+1)
			}
		case "file":
			if item.Size == nil || *item.Size < 0 {
				return errors.New("服务端缺少文件大小")
			}
			if err = r.fetchFile(ctx, progress, unit, snapshot, item.Path, local, *item.Size, item.Executable); err == nil {
				res.Files++
				res.Bytes += *item.Size
			}
		case "symlink":
			// Never create a link escaping the new restore directory.
			if filepath.IsAbs(item.Target) || filepath.VolumeName(item.Target) != "" || !inside(tmp, filepath.Join(filepath.Dir(local), item.Target)) {
				return fmt.Errorf("链接指向恢复范围外，未写入目标目录: %s", item.Path)
			}
			return os.Symlink(item.Target, local)
		default:
			return fmt.Errorf("本版无法完整还原目录项 %s (%s)", item.Path, item.Kind)
		}
		if err == nil && item.Mtime != nil {
			err = os.Chtimes(local, *item.Mtime, *item.Mtime)
		}
		return err
	}
	dir = func(remote, local string, depth int) error {
		if depth > 512 {
			return errors.New("目录嵌套过深")
		}
		p, e := r.Entries(ctx, unit, snapshot, remote)
		if e != nil {
			return e
		}
		for _, m := range p.Items {
			var item entry
			if e = json.Unmarshal(m, &item); e != nil {
				return e
			}
			name := path.Base(item.Path)
			if !localSegment(name) || path.Join(remote, name) != item.Path {
				return fmt.Errorf("无效的目录项 %q", item.Path)
			}
			if e = materialize(item, filepath.Join(local, name), depth); e != nil {
				return e
			}
		}
		return nil
	}
	if member == "" {
		e = dir("", tmp, 0)
	} else {
		parent := path.Dir(member)
		if parent == "." {
			parent = ""
		}
		pg, err := r.Entries(ctx, unit, snapshot, parent)
		if err != nil {
			return res, err
		}
		found := false
		for _, m := range pg.Items {
			var item entry
			if err = json.Unmarshal(m, &item); err != nil {
				return res, err
			}
			if item.Path == member {
				found = true
				e = materialize(item, filepath.Join(tmp, path.Base(member)), 0)
				break
			}
		}
		if !found {
			return res, errors.New("指定成员不存在")
		}
	}
	if e != nil {
		return res, e
	}
	progress.phase("finalizing")
	if e = publishNew(tmp, dst); e != nil {
		return res, e
	}
	return res, nil
}
func (r *Remote) fetchFile(ctx context.Context, progress *transfer, unit, snapshot, name, dest string, size int64, executable bool) error {
	resp, e := r.request(ctx, "GET", "/units/"+unit+"/content", url.Values{"path": {name}, "snapshotId": {snapshot}}, nil, "")
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	resp.Body = progress.download(resp.Body, name, size)
	perm := os.FileMode(0600)
	if executable {
		perm = 0700
	}
	f, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if e != nil {
		return e
	}
	_, e = copyVerified(f, resp, size)
	ce := f.Close()
	if e == nil {
		e = ce
	}
	return e
}

// Content is accepted only from one complete response with a strong content digest.
func copyVerified(f *os.File, resp *http.Response, size int64) (string, error) {
	if resp.StatusCode != http.StatusOK || size < 0 || size >= 1<<53 || resp.ContentLength != size {
		return "", errors.New("文件响应不完整或长度不符")
	}
	etag := resp.Header.Get("ETag")
	if len(etag) != 66 || etag[0] != '"' || etag[65] != '"' {
		return "", errors.New("缺少强 SHA-256 ETag")
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, size+1))
	if e != nil {
		return "", e
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if n != size || digest != etag[1:65] {
		return "", errors.New("文件大小或 SHA-256 校验失败")
	}
	return digest, f.Sync()
}

type GetResult struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Get downloads a current file, including from a read-only external unit.
func (r *Remote) Get(ctx context.Context, unit, member, destination string) (GetResult, error) {
	var res GetResult
	if e := validID(unit); e != nil {
		return res, e
	}
	if member == "" || !remotePathOK(member) || destination == "" {
		return res, errors.New("需要具体的文件成员和新目标文件")
	}
	dst, e := filepath.Abs(destination)
	if e != nil {
		return res, e
	}
	if _, e = os.Lstat(dst); !os.IsNotExist(e) {
		if e == nil {
			e = errors.New("下载目标已存在")
		}
		return res, e
	}
	progress := startTransfer(r.stateHome, "download", unit, "")
	defer progress.close()
	resp, e := r.request(ctx, "GET", "/units/"+unit+"/content", url.Values{"path": {member}}, nil, "")
	if e != nil {
		return res, e
	}
	defer resp.Body.Close()
	resp.Body = progress.download(resp.Body, member, resp.ContentLength)
	f, e := os.CreateTemp(filepath.Dir(dst), ".xinhu-download-")
	if e != nil {
		return res, e
	}
	defer os.Remove(f.Name())
	digest, e := copyVerified(f, resp, resp.ContentLength)
	closeErr := f.Close()
	if e != nil {
		return res, e
	}
	if closeErr != nil {
		return res, closeErr
	}
	progress.phase("finalizing")
	if e = publishNew(f.Name(), dst); e != nil {
		return res, e
	}
	return GetResult{dst, resp.ContentLength, digest}, syncDirectory(filepath.Dir(dst))
}
