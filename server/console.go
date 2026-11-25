package server

import (
	"database/sql"
	"embed"
	"errors"
	"net/http"
	"strings"
	"time"
)

//go:embed console/*
var consoleFiles embed.FS

const schemaConsole = `
 CREATE TABLE IF NOT EXISTS console_sessions(device TEXT PRIMARY KEY REFERENCES devices(id),expires INTEGER NOT NULL);
 PRAGMA user_version=7;
`
const consoleCookie = "__Host-xinhu-console"
const consoleLifetime = 24 * time.Hour

func consoleGate(r *http.Request) error {
	if r.TLS == nil || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return forbidden()
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != "https://"+r.Host {
		return forbidden()
	}
	return nil
}
func (s *Store) browserDevice(r *http.Request) (string, error) {
	if e := consoleGate(r); e != nil {
		return "", e
	}
	c, e := r.Cookie(consoleCookie)
	if e != nil || len(c.Value) != 43 {
		return "", unauth()
	}
	var id string
	e = s.db.QueryRowContext(r.Context(), `SELECT d.id FROM devices d JOIN console_sessions c ON c.device=d.id WHERE d.verifier=? AND d.revoked=0 AND c.expires>?`, hash([]byte(c.Value)), time.Now().Unix()).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		e = unauth()
	}
	return id, e
}
func consoleSessionCookie(value string, age int) *http.Cookie {
	return &http.Cookie{Name: consoleCookie, Value: value, Path: "/", MaxAge: age, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
}
func (s *Store) consoleRoutes(m *http.ServeMux, route func(string, bool, endpoint)) {
	static := func(filename, typ string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			browserSecurity(w)
			body, e := consoleFiles.ReadFile("console/" + filename)
			if e != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", typ)
			w.Write(body)
		}
	}
	m.HandleFunc("GET /favicon.png", static("favicon.png", "image/png"))
	m.HandleFunc("GET /{$}", static("index.html", "text/html; charset=utf-8"))
	m.HandleFunc("GET /console/{$}", static("index.html", "text/html; charset=utf-8"))
	m.HandleFunc("GET /console/console.css", static("console.css", "text/css; charset=utf-8"))
	m.HandleFunc("GET /console/console.js", static("console.js", "text/javascript; charset=utf-8"))
	route("POST /console/pair", false, func(w http.ResponseWriter, r *http.Request, _ string) error {
		browserSecurity(w)
		if e := consoleGate(r); e != nil {
			return e
		}
		if _, e := s.browserDevice(r); e == nil {
			return conflict("请先退出当前网页会话")
		}
		var b struct {
			Code string `json:"code"`
			Name string `json:"deviceName"`
		}
		if e := decode(r, &b, 4096); e != nil {
			return e
		}
		name := strings.TrimSpace(b.Name)
		if name == "" {
			name = "梦枢天罗 · 网页控制台"
		}
		id, token := newID(), secret()
		if e := s.pairDevice(r.Context(), strings.TrimSpace(b.Code), id, name, token, "", time.Now().Add(consoleLifetime).Unix()); e != nil {
			return e
		}
		http.SetCookie(w, consoleSessionCookie(token, int(consoleLifetime.Seconds())))
		return reply(w, 201, fields{"paired": true, "deviceId": id})
	})
	route("GET /console/session", false, func(w http.ResponseWriter, r *http.Request, _ string) error {
		browserSecurity(w)
		id, e := s.browserDevice(r)
		if e != nil {
			var p *problem
			if errors.As(e, &p) && p.Status == 401 {
				return reply(w, 200, fields{"paired": false})
			}
			return e
		}
		return reply(w, 200, fields{"paired": true, "deviceId": id})
	})
	route("POST /console/logout", false, func(w http.ResponseWriter, r *http.Request, _ string) error {
		browserSecurity(w)
		if e := consoleGate(r); e != nil {
			return e
		}
		id, e := s.browserDevice(r)
		if e == nil {
			if e = s.Revoke(id); e != nil {
				return e
			}
		} else {
			var p *problem
			if !errors.As(e, &p) || p.Status != 401 {
				return e
			}
		}
		http.SetCookie(w, consoleSessionCookie("", -1))
		return reply(w, 200, fields{"paired": false})
	})
}
