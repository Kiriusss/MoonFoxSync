package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type externalRoot struct{ Path, Identity string }
type nodeState struct {
	Identity      string
	Size          int64
	Kind          string
	Mtime, Change time.Time
}

func (s *Store) external(u string) (*externalRoot, error) {
	var x externalRoot
	e := s.db.QueryRow("SELECT path,identity FROM external_roots WHERE unit=?", u).Scan(&x.Path, &x.Identity)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	return &x, e
}
func containsPath(parent, child string) bool {
	p, e := filepath.Rel(parent, child)
	return e == nil && p != ".." && !strings.HasPrefix(p, ".."+string(filepath.Separator))
}
func (s *Store) AddExternal(name, root string, sensitive ...string) (string, error) {
	if !filepath.IsAbs(root) || strings.TrimSpace(name) == "" || len([]rune(name)) > 255 {
		return "", errors.New("absolute source root and nonempty unit name required")
	}
	p, e := filepath.EvalSymlinks(root)
	if e != nil {
		return "", e
	}
	if filepath.Dir(p) == p {
		return "", errors.New("select a dedicated subdirectory")
	}
	sources := append([]string{s.dir}, sensitive...)
	for _, q := range sources {
		if q == "" {
			continue
		}
		q, e = filepath.Abs(q)
		if e != nil {
			return "", e
		}
		canonical, err := filepath.EvalSymlinks(q)
		if err == nil {
			q = canonical
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if containsPath(q, p) || containsPath(p, q) {
			return "", errors.New("external source overlaps service data or configuration")
		}
	}
	rootfd, e := openRegisteredRoot(p)
	if e != nil {
		return "", e
	}
	defer rootfd.Close()
	st, e := fileState(rootfd)
	if e != nil {
		return "", e
	}
	tx, e := s.db.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	id := newID()
	if _, e = tx.Exec("INSERT INTO units(id,name) VALUES(?,?)", id, name); e != nil {
		return "", e
	}
	if _, e = tx.Exec("INSERT INTO external_roots VALUES(?,?,?)", id, p, st.Identity); e != nil {
		return "", e
	}
	return id, tx.Commit()
}
func dirChanged() *problem {
	return &problem{409, "DIRECTORY_CHANGED", "directory contents changed", "relist", nil}
}
func (s *Store) externalList(w http.ResponseWriter, r *http.Request, dev, unit, path string, x *externalRoot) error {
	f, e := x.open(path, true)
	if e != nil {
		return e
	}
	defer f.Close()
	before, e := fileState(f)
	if e != nil {
		return e
	}
	names, e := f.Readdirnames(-1)
	if e != nil {
		return e
	}
	sort.Strings(names)
	// Bound cursor validity to the directory inode and its observed mutation generation.
	scope := fmt.Sprintf("%s:%s:%s:%s:%d", dev, unit, path, before.Identity, before.Change.UnixNano())
	lim, offset, e := page(r, scope)
	if e != nil {
		if r.URL.Query().Get("cursor") != "" {
			return dirChanged()
		}
		return e
	}
	if offset > len(names) {
		return dirChanged()
	}
	end := min(len(names), offset+lim)
	items := []fields{}
	for _, name := range names[offset:end] {
		if !validPath(name) {
			continue
		}
		child, e := statChild(f, name)
		if errors.Is(e, os.ErrNotExist) {
			return dirChanged()
		}
		if e != nil {
			return e
		}
		if child.Kind == "" {
			continue
		}
		rel := name
		if path != "" {
			rel = path + "/" + name
		}
		item := fields{"path": rel, "kind": child.Kind, "size": nil, "mtime": stamp(child.Mtime)}
		if child.Kind == "file" {
			item["size"] = child.Size
		}
		items = append(items, item)
	}
	after, e := fileState(f)
	if e != nil {
		return e
	}
	if after != before {
		return dirChanged()
	}
	cursor := next(scope, offset, lim, len(names)-offset)
	return reply(w, 200, fields{"items": items, "snapshotId": nil, "nextCursor": cursor})
}

type cancellableReader struct {
	ctx context.Context
	r   io.Reader
}

func (c cancellableReader) Read(b []byte) (int, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	return c.r.Read(b)
}
func externalChanged(conditional bool) error {
	if conditional {
		return &problem{412, "SOURCE_CHANGED", "source changed; reopen the file", "refresh_source", nil}
	}
	return &problem{409, "CONFLICT", "source is not stable", "refresh_source", nil}
}
func (s *Store) externalContent(w http.ResponseWriter, r *http.Request, path string, x *externalRoot) error {
	if path == "" {
		return bad("file path required")
	}
	select {
	case s.externalSlots <- struct{}{}:
		defer func() { <-s.externalSlots }()
	default:
		w.Header().Set("Retry-After", "2")
		return &problem{429, "RATE_LIMITED", "reader slots are busy", "retry", nil}
	}
	src, e := x.open(path, false)
	if e != nil {
		return e
	}
	defer src.Close()
	before, e := fileState(src)
	if e != nil {
		return e
	}
	if before.Kind != "file" {
		return absent()
	}
	conditional := r.Header.Get("If-Match") != ""
	// An unlinked temporary file is private to this response and disappears on crash.
	frozen, e := os.CreateTemp(filepath.Join(s.dir, "incoming"), "read-")
	if e != nil {
		return e
	}
	defer frozen.Close()
	if e = os.Remove(frozen.Name()); e != nil {
		return e
	}
	if before.Size < 0 || before.Size >= 1<<53 {
		return large()
	}
	hash1 := sha256.New()
	n, e := io.Copy(io.MultiWriter(frozen, hash1), cancellableReader{r.Context(), io.LimitReader(src, before.Size+1)})
	if e != nil {
		return e
	}
	if n != before.Size {
		return externalChanged(conditional)
	}
	confirm, e := x.open(path, false)
	if e != nil {
		return externalChanged(conditional)
	}
	defer confirm.Close()
	st, e := fileState(confirm)
	if e != nil {
		return e
	}
	if st != before {
		return externalChanged(conditional)
	}
	hash2 := sha256.New()
	n, e = io.Copy(hash2, cancellableReader{r.Context(), io.LimitReader(confirm, before.Size+1)})
	if e != nil {
		return e
	}
	after, e := fileState(confirm)
	if e != nil {
		return e
	}
	if n != st.Size || after != st || !bytes.Equal(hash1.Sum(nil), hash2.Sum(nil)) {
		return externalChanged(conditional)
	}
	// The client can append response fragments only when both strong ETags agree.
	etag := fmt.Sprintf("\"%x\"", hash1.Sum(nil))
	if conditional && etag != r.Header.Get("If-Match") {
		return externalChanged(true)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etag)
	raw := r.Header.Get("Range")
	off, length, ok := parseRange(raw, st.Size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", st.Size))
		return &problem{416, "RANGE_INVALID", "invalid byte range", "fix_request", nil}
	}
	status := 200
	if raw != "" {
		status = 206
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+length-1, st.Size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		if _, e = io.CopyN(w, io.NewSectionReader(frozen, off, length), length); e != nil {
			panic(http.ErrAbortHandler)
		}
	}
	return nil
}
