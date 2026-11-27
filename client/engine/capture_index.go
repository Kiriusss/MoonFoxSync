package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

type indexedFile struct {
	stamp  []byte
	digest string
}

type reusedFile struct {
	path  string
	size  int64
	stamp []byte
}

type captureFileIndex struct {
	unit         string
	device       uint64
	stableBefore int64
	lastSave     time.Time
	directories  map[string]bool
	stale        []string
	updates      map[string]indexedFile
	reused       map[string]reusedFile
}

func (s *Store) loadCaptureIndex(ctx context.Context, b Binding, root *os.Root) (*captureFileIndex, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	device, supported := captureVolume(f)
	f.Close()
	if !supported {
		return nil, nil
	}
	index := &captureFileIndex{
		unit:   b.Unit,
		device: device,
		// Do not trust an observation made during the timestamp's tick. Some
		// local filesystems store ctime with only one-second resolution.
		stableBefore: time.Now().Add(-2 * time.Second).UnixNano(),
		lastSave:     time.Now(),
		directories:  make(map[string]bool),
		updates:      make(map[string]indexedFile),
		reused:       make(map[string]reusedFile),
	}
	// Skip directly between distinct directory keys using the primary index.
	rows, err := s.db.QueryContext(ctx, `WITH RECURSIVE dirs(parent) AS (
	SELECT MIN(parent) FROM capture_file_index WHERE unit=?1 UNION ALL
	SELECT (SELECT MIN(parent) FROM capture_file_index WHERE unit=?1 AND parent>dirs.parent) FROM dirs WHERE parent IS NOT NULL)
	SELECT parent FROM dirs WHERE parent IS NOT NULL`, b.Unit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, err
		}
		index.directories[dir] = true
	}
	return index, rows.Err()
}

func (i *captureFileIndex) directory(ctx context.Context, s *Store, dir string) (map[string]indexedFile, error) {
	if i == nil || !i.directories[dir] {
		return nil, nil
	}
	delete(i.directories, dir)
	rows, err := s.db.QueryContext(ctx, "SELECT path,signature,digest FROM capture_file_index WHERE unit=? AND parent=?", i.unit, dir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := map[string]indexedFile{}
	for rows.Next() {
		var name string
		var file indexedFile
		if err := rows.Scan(&name, &file.stamp, &file.digest); err != nil {
			return nil, err
		}
		files[name] = file
	}
	return files, rows.Err()
}

func (i *captureFileIndex) finishDirectory(ctx context.Context, s *Store, dir string, files map[string]indexedFile) error {
	for name := range files {
		i.stale = append(i.stale, filepath.Join(dir, name))
		if len(i.stale) >= 512 {
			if err := i.save(ctx, s, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *captureFileIndex) stamp(info fs.FileInfo) []byte {
	id, ok := workspaceGitIdentity(info)
	ct := fileChangeTime(info)
	if !ok || id[0] != i.device || ct <= 0 || ct >= i.stableBefore {
		return nil
	}
	b := make([]byte, 0, 40)
	for _, v := range []uint64{id[0], id[1], uint64(ct), uint64(info.ModTime().UnixNano()), uint64(info.Size())} {
		b = binary.LittleEndian.AppendUint64(b, v)
	}
	return b
}

func (i *captureFileIndex) lookup(path string, info fs.FileInfo, directory map[string]indexedFile) (string, bool) {
	if i == nil {
		return "", false
	}
	name := filepath.Base(path)
	file, ok := directory[name]
	if !ok || len(file.stamp) != 40 || !bytes.Equal(file.stamp, i.stamp(info)) || len(file.digest) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(file.digest); err != nil {
		return "", false
	}
	delete(directory, name)
	i.reused[file.digest] = reusedFile{path, info.Size(), file.stamp}
	return file.digest, true
}

func (i *captureFileIndex) record(ctx context.Context, s *Store, path string, info fs.FileInfo, digest string, directory map[string]indexedFile) error {
	if i == nil {
		return nil
	}
	if stamp := i.stamp(info); stamp != nil {
		i.updates[path] = indexedFile{stamp, digest}
		delete(directory, filepath.Base(path))
		// Stable, individually hashed files can be reused even if another file
		// changes later in a long scan or the agent is interrupted.
		if len(i.updates) >= 512 || time.Since(i.lastSave) >= 10*time.Second {
			return i.save(ctx, s, false)
		}
	}
	return nil
}

func (i *captureFileIndex) save(ctx context.Context, s *Store, complete bool) error {
	if i == nil || (len(i.updates)+len(i.stale) == 0 && (!complete || len(i.directories) == 0)) {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	update, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO capture_file_index(unit,parent,path,signature,digest) VALUES(?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer update.Close()
	for path, file := range i.updates {
		if _, err := update.ExecContext(ctx, i.unit, filepath.Dir(path), filepath.Base(path), file.stamp, file.digest); err != nil {
			return err
		}
	}
	if len(i.stale) > 0 {
		remove, err := tx.PrepareContext(ctx, "DELETE FROM capture_file_index WHERE unit=? AND parent=? AND path=?")
		if err != nil {
			return err
		}
		defer remove.Close()
		for _, path := range i.stale {
			if _, err := remove.ExecContext(ctx, i.unit, filepath.Dir(path), filepath.Base(path)); err != nil {
				return err
			}
		}
	}
	if complete {
		for dir := range i.directories {
			if _, err := tx.ExecContext(ctx, "DELETE FROM capture_file_index WHERE unit=? AND parent=?", i.unit, dir); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	clear(i.updates)
	i.stale = i.stale[:0]
	i.lastSave = time.Now()
	return nil
}

func (c *capture) hashFile(parent *os.Root, name, path string, info fs.FileInfo) (string, error) {
	f, err := parent.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !sameFile(opened, info) {
		return "", fmt.Errorf("采集期间文件已变化: %s", path)
	}
	digest, err := c.files.Build(f, c.put)
	if err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !sameFile(opened, after) {
		return "", fmt.Errorf("采集期间文件已变化: %s", path)
	}
	return digest, nil
}

// A new snapshot needs the entire transitive closure, including files whose
// bodies were not opened this time. Read the small immutable file descriptors
// and only rebuild from source if spool cleanup removed an object.
func (c *capture) collectReused() error {
	if c.index == nil {
		return nil
	}
	for digest, file := range c.index.reused {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if err := c.collectFileRefs(digest, file.size); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			info, err := c.root.Lstat(file.path)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || !bytes.Equal(c.index.stamp(info), file.stamp) {
				return fmt.Errorf("采集期间文件已变化: %s", file.path)
			}
			rebuilt, err := c.hashFile(c.root, file.path, file.path, info)
			if err != nil {
				return err
			}
			if rebuilt != digest {
				return fmt.Errorf("采集期间文件已变化: %s", file.path)
			}
		}
	}
	return nil
}

func (c *capture) collectFileRefs(digest string, size int64) error {
	info, err := objectfmt.ReadFile(digest, func(ref objectfmt.Ref) ([]byte, error) {
		data, err := c.store.readSpoolBytes(ref)
		if err == nil {
			c.refs[ref] = len(data)
		}
		return data, err
	})
	if err != nil {
		return err
	}
	if info.Size != size {
		return fmt.Errorf("缓存文件长度不一致: %s", digest)
	}
	remaining := size
	for _, ref := range info.Chunks {
		n := min(remaining, int64(objectfmt.ChunkSize))
		remaining -= n
		if _, ok := c.refs[ref]; ok {
			continue
		}
		st, err := os.Lstat(c.store.objectPath(ref))
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() || st.Size() != n {
			return fmt.Errorf("损坏的缓存对象 %s", ref.Digest)
		}
		c.refs[ref] = int(n)
	}
	return nil
}

func (c *capture) verifyChecks() error {
	var dir *os.Root
	var last string
	defer func() {
		if dir != nil {
			dir.Close()
		}
	}()
	for _, check := range c.checks {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		parent := filepath.Dir(check.path)
		if dir == nil || parent != last {
			if dir != nil {
				dir.Close()
				dir = nil
			}
			var err error
			dir, err = c.root.OpenRoot(parent)
			if err != nil {
				return err
			}
			last = parent
		}
		info, err := dir.Lstat(filepath.Base(check.path))
		if err != nil || !sameFile(info, check.info) {
			return fmt.Errorf("采集期间目录或文件已变动，请重试: %s", check.path)
		}
	}
	return c.verifyDirectories()
}
