package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type CacheEntry struct {
	Unit     string `json:"unitId"`
	Snapshot string `json:"snapshotId"`
	Path     string `json:"path"`
	State    string `json:"state"`
	SHA      string `json:"sha256"`
	Size     int64  `json:"size"`
	Local    string `json:"localPath,omitempty"`
	Hold     string `json:"-"`
}

func (s *Store) cacheDir() (string, error) {
	d := filepath.Join(s.Home, "downloads")
	if err := os.Mkdir(d, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	st, e := os.Lstat(d)
	if e != nil {
		return "", e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("下载缓存目录身份无效")
	}
	return d, nil
}
func cacheName(unit, snapshot, member string) string {
	h := sha256.Sum256([]byte(unit + "\x00" + snapshot + "\x00" + member))
	ext := filepath.Ext(member)
	if len(ext) > 12 {
		ext = ""
	}
	for _, c := range ext {
		if !strings.ContainsRune(".abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", c) {
			ext = ""
			break
		}
	}
	return hex.EncodeToString(h[:]) + ext
}
func fileHash(name string) (string, int64, error) {
	st, e := os.Lstat(name)
	if e != nil {
		return "", 0, e
	}
	if !st.Mode().IsRegular() {
		return "", 0, errors.New("缓存文件已被替换")
	}
	f, e := os.Open(name)
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !sameFile(st, opened) {
		return "", 0, errors.New("缓存文件读取前已变动")
	}
	h := sha256.New()
	n, e := io.Copy(h, f)
	if e != nil {
		return "", 0, e
	}
	after, e := os.Lstat(name)
	if e != nil || !sameFile(opened, after) {
		return "", 0, errors.New("缓存内容正在变化")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

const cacheCols = "unit,snapshot,path,state,digest,size,hold"

func scanCache(row interface{ Scan(...any) error }) (CacheEntry, error) {
	var c CacheEntry
	e := row.Scan(&c.Unit, &c.Snapshot, &c.Path, &c.State, &c.SHA, &c.Size, &c.Hold)
	return c, e
}
func (s *Store) Cached(ctx context.Context) ([]CacheEntry, error) {
	d, e := s.cacheDir()
	if e != nil {
		return nil, e
	}
	rows, e := s.db.QueryContext(ctx, "SELECT "+cacheCols+" FROM cached_files ORDER BY unit,path,snapshot")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []CacheEntry{}
	for rows.Next() {
		v, e := scanCache(rows)
		if e != nil {
			return nil, e
		}
		if v.State == "downloaded" {
			v.Local = filepath.Join(d, cacheName(v.Unit, v.Snapshot, v.Path))
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Cache(ctx context.Context, action, unit, snapshot, member string) (CacheEntry, error) {
	activity, lockErr := s.localLock(false)
	if lockErr != nil {
		return CacheEntry{}, lockErr
	}
	defer activity.Close()
	var v CacheEntry
	if e := validID(unit); e != nil {
		return v, e
	}
	if e := validID(snapshot); e != nil {
		return v, e
	}
	if !remotePathOK(member) || member == "" {
		return v, errors.New("需要具体文件的相对路径")
	}
	dir, e := s.cacheDir()
	if e != nil {
		return v, e
	}
	key := cacheName(unit, snapshot, member)
	local := filepath.Join(dir, key)
	isolated := local + ".evicting"
	lock, e := cacheLock(filepath.Join(dir, key+".lock"))
	if e != nil {
		return v, e
	}
	defer lock.Close()
	v, e = scanCache(s.db.QueryRowContext(ctx, "SELECT "+cacheCols+" FROM cached_files WHERE unit=? AND snapshot=? AND path=?", unit, snapshot, member))
	empty := errors.Is(e, sql.ErrNoRows)
	if e != nil && !empty {
		return v, e
	}
	if empty {
		v = CacheEntry{Unit: unit, Snapshot: snapshot, Path: member, Hold: newID()}
	}
	update := func(state string) error {
		_, err := s.db.ExecContext(ctx, "INSERT INTO cached_files VALUES(?,?,?,?,?,?,?) ON CONFLICT(unit,snapshot,path) DO UPDATE SET state=excluded.state,digest=excluded.digest,size=excluded.size", unit, snapshot, member, state, v.SHA, v.Size, v.Hold)
		if err == nil {
			v.State = state
			v.Local = ""
			if state == "downloaded" {
				v.Local = local
			}
		}
		return err
	}
	recoverEvict := func() error {
		st, err := os.Lstat(isolated)
		if os.IsNotExist(err) {
			if _, err = os.Lstat(local); os.IsNotExist(err) {
				return update("nas_only")
			}
			return err
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return errors.New("隔离区文件类型变化，已保留")
		}
		hash, n, err := fileHash(isolated)
		if err != nil {
			return err
		}
		if hash != v.SHA || n != v.Size {
			if err = publishNew(isolated, local); err != nil {
				return fmt.Errorf("缓存已修改，保留于 %s: %w", isolated, err)
			}
			_ = update("downloaded")
			return errors.New("缓存内容已被修改，已保留")
		}
		if err = removeReadOnly(isolated); err != nil {
			return err
		}
		if err = syncDirectory(dir); err != nil {
			return err
		}
		return update("nas_only")
	}
	if v.State == "evicting" {
		if e = recoverEvict(); e != nil {
			return v, e
		}
	}
	if action == "fetch" {
		if current, err := os.Lstat(local); err == nil {
			if current.Mode().IsRegular() && !empty {
				hash, n, err := fileHash(local)
				if err == nil && hash == v.SHA && n == v.Size {
					e = update("downloaded")
					return v, e
				}
			}
			return v, errors.New("下载位置已有未核实或修改的内容，已保留")
		} else if !os.IsNotExist(err) {
			return v, err
		}
		r, err := s.Remote(ctx)
		if err != nil {
			return v, err
		}
		parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(member)))
		if parent == "." {
			parent = ""
		}
		page, err := r.Entries(ctx, unit, snapshot, parent)
		if err != nil {
			return v, err
		}
		var size int64 = -1
		for _, raw := range page.Items {
			var x entry
			if err = json.Unmarshal(raw, &x); err != nil {
				return v, err
			}
			if x.Path == member && x.Kind == "file" && x.Size != nil {
				size = *x.Size
				break
			}
		}
		if size < 0 {
			return v, errors.New("未找到快照中的普通文件")
		}
		tmp, err := os.MkdirTemp(dir, "fetch-")
		if err != nil {
			return v, err
		}
		defer os.RemoveAll(tmp)
		file := filepath.Join(tmp, "content")
		progress := startTransfer(s.Home, "download", unit, snapshot)
		defer progress.close()
		if err = r.fetchFile(ctx, progress, unit, snapshot, member, file, size, false); err != nil {
			return v, err
		}
		progress.phase("finalizing")
		v.SHA, v.Size, err = fileHash(file)
		if err != nil {
			return v, err
		}
		if err = os.Chmod(file, 0400); err != nil {
			return v, err
		}
		if err = update("fetching"); err != nil {
			return v, err
		}
		if err = publishNew(file, local); err != nil {
			return v, err
		}
		if err = syncDirectory(dir); err != nil {
			return v, err
		}
		e = update("downloaded")
		return v, e
	}
	if action != "evict" {
		return v, errors.New("未知缓存操作")
	}
	if empty {
		return v, errors.New("文件不属于应用下载缓存")
	}
	if v.State == "nas_only" {
		return v, nil
	}
	hash, n, e := fileHash(local)
	if e != nil {
		return v, e
	}
	if hash != v.SHA || n != v.Size {
		return v, errors.New("缓存内容已修改，已保留")
	}
	r, e := s.Remote(ctx)
	if e != nil {
		return v, e
	}
	var hold struct {
		ID       string `json:"holdId"`
		Unit     string `json:"unitId"`
		Snapshot string `json:"snapshotId"`
		Path     string `json:"path"`
		State    string `json:"state"`
		Device   string `json:"deviceId"`
	}
	var device string
	if e = s.db.QueryRowContext(ctx, "SELECT device FROM account WHERE id=1").Scan(&device); e != nil {
		return v, e
	}
	if e = r.call(ctx, "PUT", "/units/"+unit+"/holds/"+v.Hold, nil, map[string]string{"snapshotId": snapshot, "path": member}, &hold); e != nil {
		return v, e
	}
	if hold.ID != v.Hold || hold.Unit != unit || hold.Snapshot != snapshot || hold.Path != member || hold.State != "active" || hold.Device != device {
		return v, errors.New("NAS 保留凭证与缓存身份不一致")
	}
	if e = update("evicting"); e != nil {
		return v, e
	}
	if e = publishNew(local, isolated); e != nil {
		return v, e
	}
	e = recoverEvict()
	return v, e
}

func removeReadOnly(p string) error {
	if runtime.GOOS == "windows" {
		if err := os.Chmod(p, 0600); err != nil {
			return err
		}
	}
	return os.Remove(p)
}
