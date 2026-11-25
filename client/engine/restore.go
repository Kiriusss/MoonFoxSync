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
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type entry struct {
	Path       string     `json:"path"`
	Kind       string     `json:"kind"`
	Size       *int64     `json:"size"`
	Mtime      *time.Time `json:"mtime"`
	Digest     string     `json:"digest"`
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
	limit, err := r.downloadLimit(ctx)
	if err != nil {
		return res, err
	}
	group, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tree := restoreTree{remote: r, progress: progress, unit: unit, snapshot: snapshot, root: tmp, limit: limit, workers: make(chan struct{}, limit)}
	if member == "" {
		err = tree.directory(group, cancel, "", tmp, 0)
	} else {
		parent := path.Dir(member)
		if parent == "." {
			parent = ""
		}
		found := false
		err = tree.visit(group, parent, func(item entry) error {
			if item.Path != member {
				return nil
			}
			if found {
				return errors.New("服务端返回了重复的成员路径")
			}
			found = true
			return tree.materialize(group, cancel, item, filepath.Join(tmp, path.Base(member)), 0)
		})
		if err == nil && !found {
			err = errors.New("指定成员不存在")
		}
	}
	if err != nil {
		cancel(err)
	}
	tree.wg.Wait()
	res.Files, res.Bytes, res.Directories = int(tree.files.Load()), tree.bytes.Load(), tree.directories
	if cause := context.Cause(group); cause != nil {
		return res, cause
	}
	if err = tree.checkLinks(); err != nil {
		return res, err
	}
	progress.phase("finalizing")
	if err = ctx.Err(); err != nil {
		return res, err
	}
	if err = publishNew(tmp, dst); err != nil {
		return res, err
	}
	return res, nil
}

type restoreTree struct {
	remote               *Remote
	progress             *transfer
	unit, snapshot, root string
	limit                int
	workers              chan struct{}
	wg                   sync.WaitGroup
	directories          int
	files, bytes         atomic.Int64
	checkTargets         []string
}

func (r *Remote) downloadLimit(ctx context.Context) (int, error) {
	if r.store == nil {
		return defaultFileTransfers, nil
	}
	cfg, err := r.store.TransferSettings(ctx)
	return cfg.Downloads, err
}

// Visit one listing page at a time, keeping wide project directories bounded.
func (t *restoreTree) visit(ctx context.Context, parent string, visit func(entry) error) error {
	q := url.Values{"path": {parent}, "snapshotId": {t.snapshot}}
	return t.remote.eachPage(ctx, "/units/"+t.unit+"/entries", q, func(pg Page) error {
		for _, raw := range pg.Items {
			var item entry
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			name := path.Base(item.Path)
			if !localSegment(name) || path.Join(parent, name) != item.Path {
				return fmt.Errorf("无效的目录项 %q", item.Path)
			}
			if err := visit(item); err != nil {
				return err
			}
		}
		return nil
	})
}

func (t *restoreTree) directory(ctx context.Context, cancel context.CancelCauseFunc, parent, local string, depth int) error {
	if depth > 512 {
		return errors.New("目录嵌套过深")
	}
	return t.visit(ctx, parent, func(item entry) error {
		return t.materialize(ctx, cancel, item, filepath.Join(local, path.Base(item.Path)), depth)
	})
}

func (t *restoreTree) materialize(ctx context.Context, cancel context.CancelCauseFunc, item entry, local string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch item.Kind {
	case "directory":
		if err := os.Mkdir(local, 0700); err != nil {
			return err
		}
		t.directories++
		if err := t.directory(ctx, cancel, item.Path, local, depth+1); err != nil {
			return err
		}
		return restoreTime(local, item.Mtime)
	case "symlink":
		if item.Target == "" || os.IsPathSeparator(item.Target[0]) || filepath.VolumeName(item.Target) != "" || !inside(t.root, filepath.Join(filepath.Dir(local), item.Target)) {
			return fmt.Errorf("链接指向恢复范围外，未写入目标目录: %s", item.Path)
		}
		if err := os.Symlink(item.Target, local); err != nil {
			return err
		}
		// Interior ".." can traverse another symlink before walking upward.
		// Recheck that path against the complete staging tree before publication.
		if strings.Contains("/"+filepath.ToSlash(item.Target)+"/", "/../") {
			rel, err := filepath.Rel(t.root, local)
			if err != nil {
				return err
			}
			t.checkTargets = append(t.checkTargets, rel)
		}
		return nil
	case "file":
		if item.Size == nil || *item.Size < 0 || *item.Size >= 1<<53 {
			return errors.New("服务端文件大小无效")
		}
		select {
		case t.workers <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		// Reserve an exclusive file during traversal. Once children are scheduled
		// the directory timestamp can be restored while their contents arrive.
		perm := os.FileMode(0600)
		if item.Executable {
			perm = 0700
		}
		f, err := os.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if err != nil {
			<-t.workers
			return err
		}
		t.wg.Go(func() {
			defer func() { <-t.workers }()
			err := t.remote.downloadTo(ctx, t.progress, t.unit, t.snapshot, item.Path, f, *item.Size, item.Digest, t.limit)
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil {
				err = restoreTime(local, item.Mtime)
			}
			if err != nil {
				cancel(err)
				return
			}
			t.files.Add(1)
			t.bytes.Add(*item.Size)
		})
		return nil
	default:
		return fmt.Errorf("本版无法完整还原目录项 %s (%s)", item.Path, item.Kind)
	}
}

func (t *restoreTree) checkLinks() error {
	if len(t.checkTargets) == 0 {
		return nil
	}
	root, err := os.OpenRoot(t.root)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, rel := range t.checkTargets {
		if _, err := root.Stat(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("无法安全还原链接 %s: %w", rel, err)
		}
	}
	return nil
}

func restoreTime(local string, stamp *time.Time) error {
	if stamp == nil {
		return nil
	}
	return os.Chtimes(local, *stamp, *stamp)
}

func (r *Remote) downloadTo(ctx context.Context, progress *transfer, unit, snapshot, name string, f *os.File, size int64, expected string, limit int) error {
	release, err := acquireFileSlot(ctx, r.stateHome, "download", limit)
	if err != nil {
		return err
	}
	defer release()
	progress.file(name, 0, size, 0)
	defer progress.finishFile(name)
	resp, err := r.request(ctx, "GET", "/units/"+unit+"/content", url.Values{"path": {name}, "snapshotId": {snapshot}}, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if expected != "" && resp.Header.Get("ETag") != "\""+expected+"\"" {
		return errors.New("文件摘要与选定快照不符")
	}
	resp.Body = progress.download(resp.Body, name, size)
	_, err = copyVerified(f, resp, size)
	return err
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
	limit, e := r.downloadLimit(ctx)
	if e != nil {
		return res, e
	}
	release, e := acquireFileSlot(ctx, r.stateHome, "download", limit)
	if e != nil {
		return res, e
	}
	defer release()
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
