package server

import (
	"database/sql"
	"embed"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed reader/*
var readerAssets embed.FS

const readerCookie = "__Host-xinhu-reader"

func sessionCookie(token string) *http.Cookie {
	// __Host- cookies cannot be injected by a sibling domain.
	return &http.Cookie{Name: readerCookie, Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
}
func browserSecurity(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func (s *Store) readerRoutes(m *http.ServeMux, route func(string, bool, endpoint)) {
	assets, err := fs.Sub(readerAssets, "reader")
	if err != nil {
		panic(err)
	}
	static := http.StripPrefix("/reader/", http.FileServer(http.FS(assets)))
	m.HandleFunc("GET /reader/{asset...}", func(w http.ResponseWriter, r *http.Request) {
		browserSecurity(w)
		switch r.PathValue("asset") {
		case "", "reader.js", "reader.css", "xinhu.png":
			static.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	browser := func(fn endpoint, auth bool) endpoint {
		return func(w http.ResponseWriter, r *http.Request, _ string) error {
			browserSecurity(w)
			if r.Method == "POST" && (r.TLS == nil || r.Header.Get("Origin") != "https://"+r.Host) {
				return forbidden()
			}
			var device string
			if auth {
				c, err := r.Cookie(readerCookie)
				if err != nil || len(c.Value) != 43 {
					return unauth()
				}
				err = s.db.QueryRowContext(r.Context(), "SELECT id FROM devices WHERE verifier=? AND revoked=0 AND maximum_role='reader'", hash([]byte(c.Value))).Scan(&device)
				if errors.Is(err, sql.ErrNoRows) {
					return unauth()
				}
				if err != nil {
					return err
				}
			}
			return fn(w, r, device)
		}
	}
	route("POST /reader/pair", false, browser(s.readerPair, false))
	route("POST /reader/logout", false, browser(s.readerLogout, true))
	route("GET /reader/api/units", false, browser(s.units, true))
	route("GET /reader/api/units/{unitId}/snapshots", false, browser(s.history, true))
	route("GET /reader/api/units/{unitId}/entries", false, browser(s.entries, true))
	route("GET /reader/api/units/{unitId}/content", false, browser(s.readerContent, true))
	route("GET /reader/api/units/{unitId}/archive", false, browser(s.archive, true))
}
func (s *Store) readerPair(w http.ResponseWriter, r *http.Request, _ string) error {
	var b struct{ Code string }
	if err := decode(r, &b, 4096); err != nil {
		return err
	}
	id, token := newID(), secret()
	if err := s.pair(r.Context(), b.Code, id, "心狐同步 · 网页阅读器", token, "reader"); err != nil {
		return err
	}
	http.SetCookie(w, sessionCookie(token))
	return reply(w, http.StatusCreated, fields{"deviceId": id})
}
func (s *Store) readerLogout(w http.ResponseWriter, _ *http.Request, dev string) error {
	if err := s.Revoke(dev); err != nil {
		return err
	}
	c := sessionCookie("")
	c.MaxAge = -1
	http.SetCookie(w, c)
	w.WriteHeader(http.StatusNoContent)
	return nil
}
func (s *Store) readerContent(w http.ResponseWriter, r *http.Request, dev string) error {
	filename := path.Base(r.URL.Query().Get("path"))
	filename = strings.Map(func(ch rune) rune {
		if ch < 32 || ch == 127 || ch == '\\' {
			return '_'
		}
		return ch
	}, filename)
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	return s.content(w, r, dev)
}
