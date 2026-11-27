package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

var ErrCodexHistoryEmpty = errors.New("没有找到含完整换行记录的 Codex 历史文件")

type codexSample struct {
	size      int64
	signature string
	volatile  bool // Open writers can defer file timestamps on Windows.
}

type codexCached struct{ path, signature, digest string }
type codexScan struct {
	c       *capture
	source  string
	updates []codexCached
}
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}

// Snapshot a fixed prefix, ignoring a writer's incomplete final row. Searching
// backwards by blocks keeps memory bounded even for a multi-megabyte single line.
func codexCompleteEnd(ctx context.Context, r io.ReaderAt, size int64) (int64, error) {
	buf := make([]byte, min(size, 64<<10))
	for end := size; end > 0; {
		if e := ctx.Err(); e != nil {
			return 0, e
		}
		start := max(int64(0), end-int64(len(buf)))
		n := int(end - start)
		if _, e := r.ReadAt(buf[:n], start); e != nil {
			return 0, e
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, nil
}
func codexRollout(name string) bool {
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") && len(name) > len("rollout-.jsonl")
}
func codexDatePart(parts []string, name string) bool {
	if len(parts) > 2 {
		return false
	}
	digits := 2
	if len(parts) == 0 {
		digits = 4
	}
	if len(name) != digits {
		return false
	}
	for i := range name {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	n, _ := strconv.Atoi(name)
	switch len(parts) {
	case 0:
		return n > 0
	case 1:
		return n >= 1 && n <= 12
	default:
		year, _ := strconv.Atoi(parts[0])
		month, _ := strconv.Atoi(parts[1])
		dt := time.Date(year, time.Month(month), n, 0, 0, 0, 0, time.UTC)
		return n > 0 && int(dt.Month()) == month
	}
}

// Rehydrate the closure using small spool metadata objects only. Never reread
// unchanged history bodies or chunk payloads to rebuild the upload manifest.
func (s *codexScan) reuse(digest string) (bool, error) {
	if digest == "" {
		return true, nil
	}
	if len(digest) != 64 {
		return false, nil
	}
	if _, e := hex.DecodeString(digest); e != nil {
		return false, nil
	}
	c := s.c
	file, e := objectfmt.ReadFile(digest, func(ref objectfmt.Ref) ([]byte, error) {
		b, e := c.store.readSpoolBytes(ref)
		if e == nil {
			c.refs[ref] = len(b)
		}
		return b, e
	})
	if e != nil {
		if os.IsNotExist(e) {
			return false, nil
		}
		return false, e
	}
	remaining := file.Size
	for _, chunk := range file.Chunks {
		size := int(min(int64(objectfmt.ChunkSize), remaining))
		remaining -= int64(size)
		if _, ok := c.refs[chunk]; !ok {
			st, e := os.Lstat(c.store.objectPath(chunk))
			if os.IsNotExist(e) {
				return false, nil
			}
			if e != nil {
				return false, e
			}
			if !st.Mode().IsRegular() || st.Size() != int64(size) {
				return false, errors.New("Codex 历史缓存块损坏")
			}
			c.refs[chunk] = size
		}
	}
	return true, nil
}

func (s *codexScan) file(parent *os.Root, rel, name string, initial os.FileInfo) (objectfmt.Entry, bool, error) {
	var none objectfmt.Entry
	if !initial.Mode().IsRegular() || codexReparse(initial) {
		return none, false, nil
	}
	f, e := openCodexFile(parent, name)
	if e != nil {
		return none, false, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return none, false, e
	}
	if !os.SameFile(initial, info) || !info.Mode().IsRegular() || codexReparse(info) {
		return none, false, errors.New("历史文件采样前已替换")
	}
	sample, safe, e := codexSignature(f)
	if e != nil || !safe {
		return none, false, e
	}
	// The fixed-size sample, cache key and file identity come from the same handle.
	var stored string
	e = s.c.store.db.QueryRowContext(s.c.ctx, "SELECT digest FROM codex_file_cache WHERE source=? AND path=? AND signature=?", s.source, rel, sample.signature).Scan(&stored)
	cached := e == nil && !sample.volatile
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return none, false, e
	}
	if cached {
		cached, e = s.reuse(stored)
		if e != nil {
			return none, false, e
		}
	}
	if !cached {
		end, e := codexCompleteEnd(s.c.ctx, f, sample.size)
		if e != nil {
			return none, false, e
		}
		stored = ""
		if end > 0 {
			stored, e = s.c.files.Build(contextReader{s.c.ctx, io.NewSectionReader(f, 0, end)}, s.c.put)
			if e != nil {
				return none, false, e
			}
			metadata, e := objectfmt.ReadFile(stored, s.c.store.readSpoolBytes)
			if e != nil {
				return none, false, e
			}
			if metadata.Size != end {
				return none, false, errors.New("历史文件采样期间被截断")
			}
		}
		now, stillSafe, e := codexSignature(f)
		if e != nil {
			return none, false, e
		}
		if !stillSafe || now.size < sample.size {
			return none, false, errors.New("历史文件采样期间被截断或链接")
		}
		if now.signature != sample.signature && now.size <= sample.size {
			return none, false, errors.New("历史文件采样期间已改写，请重新检查")
		}
		if now != sample || now.volatile {
			// Recheck the fixed prefix when append or a Windows writer was observed.
			// This never waits for the writer to release its handle.
			if end > 0 {
				metadata, e := objectfmt.ReadFile(stored, s.c.store.readSpoolBytes)
				if e != nil {
					return none, false, e
				}
				h := sha256.New()
				n, e := io.Copy(h, contextReader{s.c.ctx, io.NewSectionReader(f, 0, end)})
				if e != nil {
					return none, false, e
				}
				if n != end || hex.EncodeToString(h.Sum(nil)) != metadata.SHA256 {
					return none, false, errors.New("历史文件的完整前缀在读取期间发生变化")
				}
			}
		} else {
			s.updates = append(s.updates, codexCached{rel, sample.signature, stored})
		}
	} else {
		after, safe, err := codexSignature(f)
		if err != nil {
			return none, false, err
		}
		if !safe || after.signature != sample.signature && after.size <= sample.size {
			return none, false, errors.New("历史文件在缓存复用期间发生变化，请重新检查")
		}
	}
	current, e := parent.Lstat(name)
	if e != nil {
		return none, false, e
	}
	if codexReparse(current) || !os.SameFile(info, current) {
		return none, false, errors.New("历史文件路径在采样期间已被替换")
	}
	if stored == "" {
		return none, false, nil
	}
	return objectfmt.Entry{Name: name, Kind: "file", MtimeNS: math.MinInt64, Digest: stored}, true, nil
}
func (s *codexScan) directory(parent *os.Root, rel, name string, dates []string, archived bool, initial os.FileInfo) (objectfmt.Entry, bool, error) {
	var none objectfmt.Entry
	if !initial.IsDir() || codexReparse(initial) {
		return none, false, nil
	}
	root, e := parent.OpenRoot(name)
	if e != nil {
		return none, false, e
	}
	defer root.Close()
	handle, e := root.Open(".")
	if e != nil {
		return none, false, e
	}
	info, e := handle.Stat()
	if e != nil || !os.SameFile(info, initial) {
		handle.Close()
		return none, false, errors.New("历史目录已被替换")
	}
	names, e := handle.Readdirnames(-1)
	handle.Close()
	if e != nil {
		return none, false, e
	}
	var children []objectfmt.Entry
	for _, n := range names {
		if e = s.c.ctx.Err(); e != nil {
			return none, false, e
		}
		isRollout := (len(dates) == 3 || archived && len(dates) == 0) && codexRollout(n)
		isDate := codexDatePart(dates, n)
		if !isDate && !isRollout {
			continue
		}
		st, e := root.Lstat(n)
		if e != nil {
			return none, false, e
		}
		var item objectfmt.Entry
		var present bool
		childPath := filepath.Join(rel, n)
		if isRollout {
			item, present, e = s.file(root, childPath, n, st)
		} else {
			item, present, e = s.directory(root, childPath, n, append(dates, n), archived, st)
		}
		if e != nil {
			return none, false, e
		}
		if present {
			children = append(children, item)
		}
	}
	current, e := parent.Lstat(name)
	if e != nil {
		return none, false, e
	}
	if !os.SameFile(current, initial) || codexReparse(current) {
		return none, false, errors.New("历史目录在采样期间已被替换")
	}
	if len(children) == 0 {
		return none, false, nil
	}
	digest, e := objectfmt.BuildDir(children, s.c.put)
	return objectfmt.Entry{Name: name, Kind: "directory", MtimeNS: math.MinInt64, Digest: digest}, true, e
}
func (c *capture) codexTree() (objectfmt.Entry, error) {
	var none objectfmt.Entry
	f, e := c.root.Open(".")
	if e != nil {
		return none, e
	}
	source, e := directoryMarker(f)
	f.Close()
	if e != nil {
		return none, e
	}
	s := codexScan{c: c, source: source}
	var children []objectfmt.Entry
	// Never enumerate the Codex home itself, unknown folders, or arbitrary JSONL.
	for _, name := range []string{"history.jsonl", "session_index.jsonl", "sessions", "archived_sessions"} {
		st, e := c.root.Lstat(name)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return none, e
		}
		var child objectfmt.Entry
		var present bool
		if strings.HasSuffix(name, ".jsonl") {
			child, present, e = s.file(c.root, name, name, st)
		} else {
			child, present, e = s.directory(c.root, name, name, nil, name == "archived_sessions", st)
		}
		if e != nil {
			return none, fmt.Errorf("采集 %s: %w", name, e)
		}
		if present {
			children = append(children, child)
		}
	}
	if len(s.updates) > 0 {
		tx, e := c.store.db.BeginTx(c.ctx, nil)
		if e != nil {
			return none, e
		}
		defer tx.Rollback()
		stmt, e := tx.PrepareContext(c.ctx, "INSERT INTO codex_file_cache VALUES(?,?,?,?) ON CONFLICT(source,path) DO UPDATE SET signature=excluded.signature,digest=excluded.digest")
		if e != nil {
			return none, e
		}
		defer stmt.Close()
		for _, row := range s.updates {
			if _, e = stmt.ExecContext(c.ctx, s.source, row.path, row.signature, row.digest); e != nil {
				return none, e
			}
		}
		if e = tx.Commit(); e != nil {
			return none, e
		}
	}
	if len(children) == 0 {
		return none, ErrCodexHistoryEmpty
	}
	root, e := objectfmt.BuildDir(children, c.put)
	if e != nil {
		return none, e
	}
	return objectfmt.Entry{Name: ".", Kind: "directory", MtimeNS: math.MinInt64, Digest: root}, nil
}
