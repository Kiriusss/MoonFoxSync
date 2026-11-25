package server

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const schemaProjects = `
CREATE TABLE IF NOT EXISTS projects(unit TEXT PRIMARY KEY REFERENCES units(id),scope TEXT NOT NULL REFERENCES units(id),project_key TEXT NOT NULL,UNIQUE(scope,project_key),CHECK(scope<>unit));
CREATE VIEW IF NOT EXISTS effective_grants AS
 SELECT unit,device,role FROM grants
 UNION ALL
 SELECT p.unit,g.device,'sync' FROM projects p JOIN grants g ON g.unit=p.scope AND g.role='sync'
 WHERE NOT EXISTS(SELECT 1 FROM grants child WHERE child.unit=p.unit AND child.device=g.device);
CREATE TABLE IF NOT EXISTS project_origins(unit TEXT PRIMARY KEY REFERENCES projects(unit), source_key TEXT NOT NULL);
PRAGMA user_version=4;
`

var gitSourceKey = regexp.MustCompile("^git:[0-9a-f]{64}$")
var projectKey = regexp.MustCompile("^[A-Za-z0-9._:-]{1,160}$")

func (s *Store) projects(w http.ResponseWriter, r *http.Request, dev string) error {
	scope := r.PathValue("unitId")
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var role string
	e = tx.QueryRowContext(r.Context(), `SELECT g.role FROM grants g JOIN devices d ON d.id=g.device AND d.revoked=0
 WHERE g.unit=? AND g.device=? AND NOT EXISTS(SELECT 1 FROM external_roots WHERE unit=g.unit)
 AND NOT EXISTS(SELECT 1 FROM projects WHERE unit=g.unit)`, scope, dev).Scan(&role)
	if errors.Is(e, sql.ErrNoRows) {
		return absent()
	}
	if e != nil {
		return e
	}
	if role != "sync" {
		return forbidden()
	}
	if r.Method == "POST" {
		var doc struct{ Key, Name, SourceKey string }
		if e = decode(r, &doc, 4096); e != nil {
			return e
		}
		doc.Name = strings.TrimSpace(doc.Name)
		if (doc.SourceKey != "" && !gitSourceKey.MatchString(doc.SourceKey)) || !projectKey.MatchString(doc.Key) || doc.Name == "" || utf8.RuneCountInString(doc.Name) > 255 || strings.IndexFunc(doc.Name, unicode.IsControl) >= 0 {
			return bad("invalid project key or display name")
		}
		var id, name, origin string
		var head *string
		e = tx.QueryRowContext(r.Context(), `SELECT u.id,u.name,u.head,COALESCE(o.source_key,'') FROM projects p JOIN units u ON u.id=p.unit LEFT JOIN project_origins o ON o.unit=p.unit WHERE p.scope=? AND p.project_key=?`, scope, doc.Key).Scan(&id, &name, &head, &origin)
		created := errors.Is(e, sql.ErrNoRows)
		if created {
			id, name = newID(), doc.Name
			if _, e = tx.ExecContext(r.Context(), "INSERT INTO units(id,name) VALUES(?,?)", id, name); e != nil {
				return e
			}
			if _, e = tx.ExecContext(r.Context(), "INSERT INTO projects VALUES(?,?,?)", id, scope, doc.Key); e != nil {
				return e
			}
			origin = doc.SourceKey
			if origin != "" {
				if _, e = tx.ExecContext(r.Context(), "INSERT INTO project_origins(unit,source_key) VALUES(?,?)", id, origin); e != nil {
					return e
				}
			}
		} else if e != nil {
			return e
		} else {
			if e = tx.QueryRowContext(r.Context(), "SELECT role FROM effective_grants WHERE unit=? AND device=?", id, dev).Scan(&role); e != nil {
				return e
			}
			if role != "sync" {
				return forbidden()
			}
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		return reply(w, status, fields{"unitId": id, "scopeId": scope, "key": doc.Key, "sourceKey": origin, "name": name, "head": head, "created": created})
	}
	limit, e := limitOf(r)
	if e != nil {
		return e
	}
	seq, rights, e := readBoundary(tx, dev)
	if e != nil {
		return e
	}
	c, e := s.parseCursor(r.URL.Query().Get("cursor"), dev+":projects:"+scope, rights, seq)
	if e != nil {
		return e
	}
	rows, e := tx.QueryContext(r.Context(), `SELECT p.unit,p.project_key,u.name,COALESCE(o.source_key,''),(SELECT s.id FROM visible_snapshots s WHERE s.unit=p.unit AND s.seq<=? AND json_extract(s.record,'$.outcome')='published' ORDER BY s.seq DESC LIMIT 1) FROM projects p JOIN units u ON u.id=p.unit LEFT JOIN project_origins o ON o.unit=p.unit WHERE p.scope=? ORDER BY p.unit LIMIT ? OFFSET ?`, c.Cut, scope, limit+1, c.Offset)
	if e != nil {
		return e
	}
	defer rows.Close()
	out := []fields{}
	for rows.Next() {
		var id, key, name, origin string
		var head *string
		if e = rows.Scan(&id, &key, &name, &origin, &head); e != nil {
			return e
		}
		out = append(out, fields{"unitId": id, "scopeId": scope, "key": key, "sourceKey": origin, "name": name, "head": head})
	}
	if e = rows.Err(); e != nil {
		return e
	}
	cursor := s.continuation(c, limit, len(out))
	if len(out) > limit {
		out = out[:limit]
	}
	return reply(w, 200, fields{"items": out, "nextCursor": cursor})
}
