package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func (s *Store) snapshot(w http.ResponseWriter, r *http.Request, dev string) error {
	unit := r.PathValue("unitId")
	if e := s.access(unit, dev, false); e != nil {
		return e
	}
	var b []byte
	e := s.db.QueryRow("SELECT record FROM timeline_history WHERE unit=? AND id=?", unit, r.PathValue("snapshotId")).Scan(&b)
	if e != nil {
		return e
	}
	if r.URL.Query().Get("fingerprint") == "true" {
		v, err := s.snapshotWithFingerprint(r.Context(), b)
		if err != nil {
			return err
		}
		return reply(w, 200, v)
	}
	return reply(w, 200, json.RawMessage(b))
}
func (s *Store) history(w http.ResponseWriter, r *http.Request, dev string) error {
	unit := r.PathValue("unitId")
	if e := s.access(unit, dev, false); e != nil {
		return e
	}
	kind := r.URL.Query().Get("kind")
	if kind == "all" {
		kind = ""
	}
	if kind != "" && kind != "published" && kind != "fork" {
		return bad("invalid snapshot filter")
	}
	lim, e := limitOf(r)
	if e != nil {
		return e
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	cut, rights, e := readBoundary(tx, dev)
	if e != nil {
		return e
	}
	c, e := s.parseCursor(r.URL.Query().Get("cursor"), dev+":history:"+unit+":"+kind, rights, cut)
	if e != nil {
		return e
	}
	rows, e := tx.QueryContext(r.Context(), "SELECT record FROM timeline_history WHERE unit=? AND seq<=? AND (?='' OR json_extract(record,'$.outcome')=?) ORDER BY seq DESC LIMIT ? OFFSET ?", unit, c.Cut, kind, kind, lim+1, c.Offset)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return e
		}
		out = append(out, b)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	cursor := s.continuation(c, lim, len(out))
	if len(out) > lim {
		out = out[:lim]
	}
	return reply(w, 200, fields{"items": out, "nextCursor": cursor})
}
func validPath(p string) bool {
	if p == "" {
		return true
	}
	if !utf8.ValidString(p) || len(p) > 16384 || strings.ContainsRune(p, 0) {
		return false
	}
	for _, n := range strings.Split(p, "/") {
		if n == "" || n == "." || n == ".." || len(n) > 255 {
			return false
		}
	}
	return true
}
func (s *Store) lookup(unit, snap, p string) (objectfmt.Entry, error) {
	var found objectfmt.Entry
	if !validPath(p) || !validID.MatchString(snap) {
		return found, bad("invalid snapshot or relative path")
	}
	found.Kind = "directory"
	e := s.db.QueryRow("SELECT root FROM visible_snapshots WHERE unit=? AND id=?", unit, snap).Scan(&found.Digest)
	if e != nil || p == "" {
		return found, e
	}
	for _, name := range strings.Split(p, "/") {
		if found.Kind != "directory" {
			return found, absent()
		}
		members, e := objectfmt.ReadDir(found.Digest, s.read)
		if e != nil {
			return found, e
		}
		i := sort.Search(len(members), func(i int) bool { return members[i].Name >= name })
		if i == len(members) || members[i].Name != name {
			return found, absent()
		}
		found = members[i]
	}
	return found, nil
}
func (s *Store) entries(w http.ResponseWriter, r *http.Request, dev string) error {
	unit := r.PathValue("unitId")
	if e := s.access(unit, dev, false); e != nil {
		return e
	}
	q := r.URL.Query()
	snap, p := q.Get("snapshotId"), q.Get("path")
	if x, e := s.external(unit); e != nil {
		return e
	} else if x != nil {
		if _, has := q["snapshotId"]; has {
			return bad("external units do not have snapshots")
		}
		return s.externalList(w, r, dev, unit, p, x)
	}
	entry, e := s.lookup(unit, snap, p)
	if e != nil {
		return e
	}
	if entry.Kind != "directory" {
		return bad("not a directory")
	}
	scope := dev + ":" + unit + ":" + snap + ":" + p
	foldersFirst := q.Get("order") == "folders"
	if foldersFirst {
		scope += "\x00folders"
	}
	lim, start, e := page(r, scope)
	if e != nil {
		return e
	}
	children, e := objectfmt.ReadDir(entry.Digest, s.read)
	if e != nil {
		return e
	}
	if foldersFirst {
		sort.SliceStable(children, func(i, j int) bool {
			return children[i].Kind == "directory" && children[j].Kind != "directory"
		})
	}
	if start > len(children) {
		return bad("invalid page offset")
	}
	last := min(start+lim, len(children))
	out := []fields{}
	for _, c := range children[start:last] {
		path := c.Name
		if p != "" {
			path = p + "/" + path
		}
		v := fields{"path": path, "kind": c.Kind, "mtime": nil, "size": nil}
		if c.MtimeNS != math.MinInt64 {
			v["mtime"] = stamp(time.Unix(0, c.MtimeNS))
		}
		switch c.Kind {
		case "file":
			fi, e := objectfmt.ReadFile(c.Digest, s.read)
			if e != nil {
				return e
			}
			v["size"] = fi.Size
			v["digest"] = fi.SHA256
			v["executable"] = c.Executable
		case "symlink":
			v["target"] = c.Target
		}
		out = append(out, v)
	}
	return reply(w, 200, fields{"snapshotId": snap, "items": out, "nextCursor": next(scope, start, lim, len(children)-start)})
}
func parseRange(raw string, total int64) (int64, int64, bool) {
	if raw == "" {
		return 0, total, true
	}
	start, end, ok := strings.Cut(strings.TrimPrefix(raw, "bytes="), "-")
	if !strings.HasPrefix(raw, "bytes=") || !ok || total == 0 {
		return 0, 0, false
	}
	parse := func(s string) (int64, bool) {
		if s == "" {
			return 0, false
		}
		for _, b := range s {
			if b < '0' || b > '9' {
				return 0, false
			}
		}
		n, e := strconv.ParseInt(s, 10, 64)
		return n, e == nil
	}
	if start == "" {
		suffix, ok := parse(end)
		if !ok || suffix <= 0 {
			return 0, 0, false
		}
		n := min(total, suffix)
		return total - n, n, true
	}
	off, ok := parse(start)
	if !ok || off >= total {
		return 0, 0, false
	}
	stop := total - 1
	if end != "" {
		n, ok := parse(end)
		if !ok || n < off {
			return 0, 0, false
		}
		stop = min(stop, n)
	}
	return off, stop - off + 1, true
}
func (s *Store) serveFile(w http.ResponseWriter, r *http.Request, digest string) error {
	f, e := objectfmt.ReadFile(digest, s.read)
	if e != nil {
		return e
	}
	etag := `"` + f.SHA256 + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	if expected := r.Header.Get("If-Match"); expected != "" && expected != etag {
		return &problem{412, "SOURCE_CHANGED", "file version differs", "refresh_source", nil}
	}
	raw := r.Header.Get("Range")
	off, n, ok := parseRange(raw, f.Size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", f.Size))
		return &problem{416, "RANGE_INVALID", "invalid byte range", "fix_request", nil}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	status := 200
	if raw != "" {
		status = 206
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, f.Size))
	}
	w.WriteHeader(status)
	if r.Method == "HEAD" {
		return nil
	}
	if e = objectfmt.WriteRange(f, off, n, w, s.read); e != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}
func (s *Store) content(w http.ResponseWriter, r *http.Request, dev string) error {
	u := r.PathValue("unitId")
	if e := s.access(u, dev, false); e != nil {
		return e
	}
	if x, e := s.external(u); e != nil {
		return e
	} else if x != nil {
		if r.URL.Query().Has("snapshotId") {
			return bad("external units do not have snapshots")
		}
		return s.externalContent(w, r, r.URL.Query().Get("path"), x)
	}
	entry, e := s.lookup(u, r.URL.Query().Get("snapshotId"), r.URL.Query().Get("path"))
	if e != nil {
		return e
	}
	if entry.Kind != "file" {
		return absent()
	}
	return s.serveFile(w, r, entry.Digest)
}
func (s *Store) attachment(w http.ResponseWriter, r *http.Request, dev string) error {
	u := r.PathValue("unitId")
	if e := s.access(u, dev, true); e != nil {
		return e
	}
	var b []byte
	if e := s.db.QueryRow("SELECT record FROM visible_snapshots WHERE unit=? AND id=?", u, r.PathValue("snapshotId")).Scan(&b); e != nil {
		return e
	}
	var snap struct{ Attachments []asset }
	if e := json.Unmarshal(b, &snap); e != nil {
		return e
	}
	for _, a := range snap.Attachments {
		if a.Name == r.PathValue("name") {
			return s.serveFile(w, r, a.FileDigest)
		}
	}
	return absent()
}
func (s *Store) object(w http.ResponseWriter, r *http.Request, dev string) error {
	u := r.PathValue("unitId")
	if e := s.access(u, dev, true); e != nil {
		return e
	}
	o := objectfmt.Ref{Kind: r.URL.Query().Get("kind"), Digest: r.PathValue("digest")}
	if !goodRef(o) {
		return bad("invalid object ref")
	}
	var ok bool
	if e := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM visible_refs WHERE unit=? AND snapshot=? AND kind=? AND digest=?)", u, r.PathValue("snapshotId"), o.Kind, o.Digest).Scan(&ok); e != nil {
		return e
	}
	if !ok {
		return absent()
	}
	b, e := s.read(o)
	if e != nil {
		return e
	}
	w.Header().Set("ETag", `"`+o.Digest+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, e = w.Write(b)
	return e
}
func (s *Store) hold(w http.ResponseWriter, r *http.Request, dev string) error {
	unit, id := r.PathValue("unitId"), r.PathValue("holdId")
	if e := s.access(unit, dev, true); e != nil {
		return e
	}
	if !validID.MatchString(id) {
		return bad("invalid holdId")
	}
	if r.Method == "PUT" {
		var b struct{ SnapshotId, Path string }
		if e := decode(r, &b, 1<<16); e != nil {
			return e
		}
		entry, e := s.lookup(unit, b.SnapshotId, b.Path)
		if e != nil {
			return e
		}
		switch entry.Kind {
		case "directory":
			e = objectfmt.VerifyTree(entry.Digest, s.read)
		case "file":
			e = objectfmt.VerifyFile(entry.Digest, s.read)
		}
		if e != nil {
			return e
		}
		if b.Path == "" {
			var raw []byte
			if e = s.db.QueryRow("SELECT record FROM visible_snapshots WHERE unit=? AND id=?", unit, b.SnapshotId).Scan(&raw); e != nil {
				return e
			}
			var stored struct{ Attachments []asset }
			if e = json.Unmarshal(raw, &stored); e != nil {
				return e
			}
			for _, attached := range stored.Attachments {
				if e = objectfmt.VerifyFile(attached.FileDigest, s.read); e != nil {
					return e
				}
			}
		}
		// Pin only if the point is still visible and the device still has its
		// sync role after the integrity check, atomically with retention/delete.
		tx, e := s.db.BeginTx(r.Context(), nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = timelineHead(r.Context(), tx, unit, dev, true); e != nil {
			return e
		}
		_, e = tx.Exec("INSERT OR IGNORE INTO holds(id,unit,snapshot,path,device,created) SELECT ?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM visible_snapshots WHERE unit=? AND id=?)", id, unit, b.SnapshotId, b.Path, dev, stamp(time.Now()), unit, b.SnapshotId)
		if e != nil {
			return e
		}
		var oldSnap, oldPath string
		var released bool
		e = tx.QueryRow("SELECT snapshot,path,released FROM holds WHERE id=? AND unit=? AND device=?", id, unit, dev).Scan(&oldSnap, &oldPath, &released)
		if e != nil {
			return e
		}
		if released || oldSnap != b.SnapshotId || oldPath != b.Path {
			return conflict("hold id already bound")
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if r.Method == "DELETE" {
		tx, e := s.db.BeginTx(r.Context(), nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = timelineHead(r.Context(), tx, unit, dev, true); e != nil {
			return e
		}
		res, e := tx.Exec("UPDATE holds SET released=1 WHERE id=? AND unit=? AND device=?", id, unit, dev)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return absent()
		}
		if e = s.applyPolicy(r.Context(), tx, unit, time.Now()); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		w.WriteHeader(204)
		return nil
	}
	var snap, path, created string
	var released bool
	e := s.db.QueryRow("SELECT snapshot,path,created,released FROM holds h WHERE id=? AND unit=? AND device=? AND NOT EXISTS(SELECT 1 FROM snapshot_tombstones t WHERE t.unit=h.unit AND t.snapshot=h.snapshot)", id, unit, dev).Scan(&snap, &path, &created, &released)
	if errors.Is(e, sql.ErrNoRows) {
		return absent()
	}
	if e != nil {
		return e
	}
	state := "active"
	if released {
		state = "released"
	}
	return reply(w, 200, fields{"holdId": id, "unitId": unit, "snapshotId": snap, "path": path, "deviceId": dev, "state": state, "createdAt": created})
}
