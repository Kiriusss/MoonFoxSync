package server

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

type problem struct {
	Status  int             `json:"-"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Action  string          `json:"action"`
	Missing []objectfmt.Ref `json:"missing,omitempty"`
}

func (p *problem) Error() string { return p.Message }
func bad(m string) *problem      { return &problem{400, "BAD_REQUEST", m, "fix_request", nil} }
func unauth() *problem {
	return &problem{401, "UNAUTHENTICATED", "authentication required", "reauthenticate", nil}
}
func forbidden() *problem        { return &problem{403, "FORBIDDEN", "permission denied", "stop", nil} }
func absent() *problem           { return &problem{404, "NOT_FOUND", "resource unavailable", "stop", nil} }
func conflict(m string) *problem { return &problem{409, "CONFLICT", m, "fix_request", nil} }
func invalid(m string) *problem  { return &problem{422, "INVALID_OBJECT", m, "fix_request", nil} }
func large() *problem {
	return &problem{413, "TOO_LARGE", "request exceeds protocol limit", "fix_request", nil}
}

type fields = map[string]any

func reply(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any, limit int64) error {
	if r.ContentLength > limit {
		return large()
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if e != nil {
		return bad("unable to read body")
	}
	if int64(len(b)) > limit {
		return large()
	}
	if !utf8.Valid(b) {
		return bad("invalid UTF-8")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return bad("invalid JSON fields")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return bad("trailing JSON data")
	}
	return nil
}

type endpoint func(http.ResponseWriter, *http.Request, string) error

func (s *Store) Handler() http.Handler {
	m := http.NewServeMux()
	route := func(pattern string, auth bool, fn endpoint) {
		m.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			var e error
			var id string
			if !s.volumeOK() {
				e = &problem{503, "VOLUME_UNAVAILABLE", "data volume unavailable", "retry", nil}
			}
			if e == nil && auth {
				token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
				if r.Header.Get("Authorization") == "" {
					browserSecurity(w)
					id, e = s.browserDevice(r)
					if e == nil && pattern == "GET /v1/units/{unitId}/content" {
						e = s.readerContent(w, r, id)
						if e == nil {
							return
						}
					}
				} else if !ok || len(token) != 43 {
					e = unauth()
				} else {
					e = s.db.QueryRowContext(r.Context(), "SELECT id FROM devices WHERE verifier=? AND revoked=0 AND NOT EXISTS(SELECT 1 FROM console_sessions c WHERE c.device=devices.id)", hash([]byte(token))).Scan(&id)
					if errors.Is(e, sql.ErrNoRows) {
						e = unauth()
					}
				}
			}
			if e == nil {
				e = fn(w, r, id)
			}
			if e != nil {
				var p *problem
				if !errors.As(e, &p) {
					if errors.Is(e, sql.ErrNoRows) {
						p = absent()
					} else if errors.Is(e, syscall.ENOSPC) || strings.Contains(e.Error(), "database or disk is full") {
						p = &problem{507, "INSUFFICIENT_SPACE", "storage is full", "wait_for_space", nil}
					} else {
						log.Printf("梦枢天罗 %s: %v", pattern, e)
						p = &problem{500, "INTERNAL", "internal service error", "retry", nil}
					}
				}
				_ = reply(w, p.Status, p)
			}
		})
	}
	route("POST /v1/pair", false, s.enroll)
	route("GET /v1/units/{unitId}/projects", true, s.projects)
	route("POST /v1/units/{unitId}/projects", true, s.projects)
	route("GET /v1/units", true, s.units)
	route("GET /v1/units/{unitId}/delete-plan", true, s.deleteLibrary)
	route("POST /v1/units/{unitId}/delete", true, s.deleteLibrary)
	route("GET /v1/changes", true, s.changes)
	route("POST /v1/units", true, func(w http.ResponseWriter, r *http.Request, dev string) error { return forbidden() })
	route("POST /v1/units/{unitId}/uploads", true, s.begin)
	route("GET /v1/uploads/{uploadId}", true, s.session)
	route("POST /v1/uploads/{uploadId}/missing", true, s.missing)
	route("POST /v1/uploads/{uploadId}/batch", true, s.batch)
	route("PUT /v1/uploads/{uploadId}/objects/{digest}", true, s.put)
	route("POST /v1/uploads/{uploadId}/finalize", true, s.finalize)
	route("GET /v1/units/{unitId}/snapshots", true, s.history)
	route("GET /v1/units/{unitId}/diff", true, s.diff)
	route("GET /v1/units/{unitId}/snapshots/{snapshotId}", true, s.snapshot)
	route("GET /v1/units/{unitId}/history-policy", true, s.historyPolicy)
	route("PUT /v1/units/{unitId}/history-policy", true, s.historyPolicy)
	route("POST /v1/units/{unitId}/history-policy/preview", true, s.historyPolicy)
	route("POST /v1/units/{unitId}/timeline/delete/preview", true, s.previewDelete)
	route("POST /v1/units/{unitId}/timeline/delete", true, s.deletePoint)
	route("POST /v1/units/{unitId}/timeline/restore", true, s.restorePoint)
	route("GET /v1/units/{unitId}/merge/preview", true, s.mergeMemberPreview)
	route("GET /v1/units/{unitId}/entries", true, s.entries)
	route("GET /v1/units/{unitId}/content", true, s.content)
	route("GET /v1/units/{unitId}/archive", true, s.archive)
	route("GET /v1/units/{unitId}/snapshots/{snapshotId}/objects/{digest}", true, s.object)
	route("GET /v1/units/{unitId}/snapshots/{snapshotId}/attachments/{name}", true, s.attachment)
	route("PUT /v1/units/{unitId}/holds/{holdId}", true, s.hold)
	route("GET /v1/units/{unitId}/holds/{holdId}", true, s.hold)
	route("DELETE /v1/units/{unitId}/holds/{holdId}", true, s.hold)
	route("POST /v1/units/{unitId}/merge/plan", true, s.mergePreview)
	route("POST /v1/units/{unitId}/merge/commit", true, s.mergeCommit)
	s.readerRoutes(m, route)
	s.consoleRoutes(m, route)
	return m
}
func (s *Store) enroll(w http.ResponseWriter, r *http.Request, _ string) error {
	var a struct{ Code, DeviceId, DeviceName, Token string }
	if e := decode(r, &a, 4096); e != nil {
		return e
	}
	if e := s.pair(r.Context(), a.Code, a.DeviceId, a.DeviceName, a.Token, ""); e != nil {
		return e
	}
	return reply(w, 201, fields{"deviceId": a.DeviceId, "canCreateUnits": false})
}
func (s *Store) access(unit, device string, write bool) error {
	var role string
	e := s.db.QueryRow("SELECT CASE WHEN EXISTS(SELECT 1 FROM external_roots x WHERE x.unit=g.unit) THEN 'reader' ELSE g.role END FROM effective_grants g JOIN devices d ON d.id=g.device WHERE unit=? AND device=? AND revoked=0", unit, device).Scan(&role)
	if errors.Is(e, sql.ErrNoRows) {
		return absent()
	}
	if e == nil && write && role != "sync" {
		return forbidden()
	}
	return e
}
func page(r *http.Request, scope string) (int, int, error) {
	lim := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		var e error
		lim, e = strconv.Atoi(v)
		if e != nil || lim < 1 || lim > 256 {
			return 0, 0, bad("invalid limit")
		}
	}
	cursor := r.URL.Query().Get("cursor")
	if cursor == "" {
		return lim, 0, nil
	}
	b, e := base64.RawURLEncoding.DecodeString(cursor)
	prefix := hash([]byte(scope)) + ":"
	v, ok := strings.CutPrefix(string(b), prefix)
	n, ne := strconv.Atoi(v)
	if e != nil || ne != nil || !ok || n < 0 {
		return 0, 0, bad("invalid cursor")
	}
	return lim, n, nil
}
func next(scope string, start, limit, count int) any {
	if count <= limit {
		return nil
	}
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", hash([]byte(scope)), start+limit)))
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
