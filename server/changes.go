package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

type frozenCursor struct {
	Scope  string
	Grants string
	Cut    int64
	Offset int
}

func limitOf(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 100, nil
	}
	n, e := strconv.Atoi(v)
	if e != nil || n < 1 || n > 256 {
		return 0, bad("invalid limit")
	}
	return n, nil
}
func readBoundary(tx *sql.Tx, dev string) (int64, string, error) {
	var seq int64
	var grants string
	e := tx.QueryRow("SELECT COALESCE(MAX(seq),0) FROM snapshots").Scan(&seq)
	if e != nil {
		return 0, "", e
	}
	e = tx.QueryRow("SELECT COALESCE(group_concat(unit||role,',' ORDER BY unit),'') FROM effective_grants WHERE device=?", dev).Scan(&grants)
	if e != nil {
		return 0, "", e
	}
	var removed int64
	e = tx.QueryRow("SELECT coalesce(max(t.rowid),0) FROM snapshot_tombstones t JOIN effective_grants g ON g.unit=t.unit WHERE g.device=?", dev).Scan(&removed)
	// Existing pre-deletion cursors remain valid until the first relevant removal.
	if removed > 0 {
		grants += ":history:" + strconv.FormatInt(removed, 10)
	}
	return seq, hash([]byte(grants)), e
}
func (s *Store) encodeCursor(c frozenCursor) string {
	raw, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, []byte(s.marker))
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Store) parseCursor(token, scope, rights string, now int64) (frozenCursor, error) {
	c := frozenCursor{Scope: scope, Grants: rights, Cut: now}
	if token == "" {
		return c, nil
	}
	exp := &problem{410, "CURSOR_EXPIRED", "refresh listing", "relist", nil}
	body, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 2048 {
		return c, exp
	}
	raw, e := base64.RawURLEncoding.DecodeString(body)
	signature, er := base64.RawURLEncoding.DecodeString(sig)
	if e != nil || er != nil {
		return c, exp
	}
	mac := hmac.New(sha256.New, []byte(s.marker))
	mac.Write(raw)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return c, exp
	}
	var parsed frozenCursor
	if json.Unmarshal(raw, &parsed) != nil || parsed.Scope != scope || parsed.Grants != rights || parsed.Cut > now || parsed.Offset < 0 {
		return c, exp
	}
	return parsed, nil
}
func (s *Store) continuation(c frozenCursor, limit, count int) any {
	if count <= limit {
		return nil
	}
	c.Offset += limit
	return s.encodeCursor(c)
}
func (s *Store) units(w http.ResponseWriter, r *http.Request, dev string) error {
	lim, e := limitOf(r)
	if e != nil {
		return e
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	high, rights, e := readBoundary(tx, dev)
	if e != nil {
		return e
	}
	c, e := s.parseCursor(r.URL.Query().Get("cursor"), dev+":units", rights, high)
	if e != nil {
		return e
	}
	rows, e := tx.QueryContext(r.Context(), `SELECT u.id,u.name,g.role,CASE WHEN x.unit IS NULL THEN 'managed' ELSE 'external' END,p.scope,p.project_key,(SELECT id FROM visible_snapshots WHERE unit=u.id AND seq<=? AND json_extract(record,'$.outcome')='published' ORDER BY seq DESC LIMIT 1)
 FROM units u LEFT JOIN projects p ON p.unit=u.id LEFT JOIN external_roots x ON x.unit=u.id JOIN effective_grants g ON u.id=g.unit WHERE g.device=? ORDER BY u.id LIMIT ? OFFSET ?`, c.Cut, dev, lim+1, c.Offset)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []fields{}
	for rows.Next() {
		var id, name, role, kind string
		var head, scope, key *string
		if e = rows.Scan(&id, &name, &role, &kind, &scope, &key, &head); e != nil {
			return e
		}
		item := fields{"unitId": id, "name": name, "role": role, "kind": kind, "head": head}
		if scope != nil {
			item["scopeId"], item["projectKey"] = *scope, *key
		}
		out = append(out, item)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	cursor := s.continuation(c, lim, len(out))
	if len(out) > lim {
		out = out[:lim]
	}
	return reply(w, 200, fields{"items": out, "nextCursor": cursor, "syncCursor": s.encodeCursor(frozenCursor{Scope: dev + ":changes", Grants: rights, Cut: c.Cut})})
}
func (s *Store) changes(w http.ResponseWriter, r *http.Request, dev string) error {
	lim, e := limitOf(r)
	if e != nil {
		return e
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	high, rights, e := readBoundary(tx, dev)
	if e != nil {
		return e
	}
	c, e := s.parseCursor(r.URL.Query().Get("cursor"), dev+":changes", rights, high)
	if e != nil {
		return e
	}
	rows, e := tx.QueryContext(r.Context(), `SELECT s.seq,s.unit,s.id,json_extract(s.record,'$.outcome') FROM visible_snapshots s JOIN effective_grants g ON g.unit=s.unit WHERE g.device=? AND s.seq>? ORDER BY s.seq LIMIT ?`, dev, c.Cut, lim+1)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []fields{}
	more := false
	for rows.Next() {
		var seq int64
		var unit, snap, kind string
		if e = rows.Scan(&seq, &unit, &snap, &kind); e != nil {
			return e
		}
		if len(out) == lim {
			more = true
			break
		}
		if kind == "fork" {
			kind = "fork_saved"
		} else {
			kind = "snapshot_published"
		}
		out = append(out, fields{"unitId": unit, "snapshotId": snap, "type": kind})
		c.Cut = seq
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if !more {
		c.Cut = high
	}
	return reply(w, 200, fields{"items": out, "cursor": s.encodeCursor(c), "hasMore": more})
}
