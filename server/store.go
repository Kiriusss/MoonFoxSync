// Package server implements the private NAS service for 心狐同步.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Config struct {
	FormatVersion int      `json:"formatVersion"`
	DataDir       string   `json:"dataDir"`
	Listen        []string `json:"listen"`
	TLS           struct {
		CertFile string `json:"certFile"`
		KeyFile  string `json:"keyFile"`
	} `json:"tls"`
}

var validID = regexp.MustCompile("^[0-7][0-9A-HJKMNP-TV-Z]{25}$")
var validDigest = regexp.MustCompile("^[0-9a-f]{64}$")

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[6:])
	t := time.Now().UnixMilli()
	for i := 5; i >= 0; i-- {
		b[i] = byte(t)
		t >>= 8
	}
	n := new(big.Int).SetBytes(b[:])
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		out[i] = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"[n.Uint64()&31]
		n.Rsh(n, 5)
	}
	return string(out)
}
func secret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func hash(b []byte) string { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }

const schema = `
CREATE TABLE meta(storeid TEXT NOT NULL);
CREATE TABLE devices(id TEXT PRIMARY KEY, name TEXT NOT NULL, verifier TEXT NOT NULL UNIQUE, maximum_role TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE units(id TEXT PRIMARY KEY, name TEXT NOT NULL, head TEXT);
CREATE TABLE grants(unit TEXT NOT NULL REFERENCES units(id), device TEXT NOT NULL REFERENCES devices(id), role TEXT NOT NULL, PRIMARY KEY(unit,device));
CREATE TABLE codes(digest TEXT PRIMARY KEY, unit TEXT NOT NULL REFERENCES units(id), role TEXT NOT NULL, expires INTEGER NOT NULL);
CREATE TABLE uploads(id TEXT PRIMARY KEY, unit TEXT NOT NULL REFERENCES units(id), device TEXT NOT NULL REFERENCES devices(id), request TEXT NOT NULL, snapshot TEXT NOT NULL, draft BLOB NOT NULL, expires INTEGER NOT NULL, receipt BLOB, UNIQUE(device,request), UNIQUE(unit,snapshot));
CREATE TABLE offers(upload TEXT NOT NULL REFERENCES uploads(id), kind TEXT NOT NULL, digest TEXT NOT NULL, size INTEGER NOT NULL, ready INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(upload,kind,digest));
CREATE TABLE snapshots(seq INTEGER PRIMARY KEY AUTOINCREMENT,unit TEXT NOT NULL REFERENCES units(id),id TEXT NOT NULL,root TEXT NOT NULL, record BLOB NOT NULL, UNIQUE(unit,id));
CREATE TABLE refs(unit TEXT NOT NULL, snapshot TEXT NOT NULL, kind TEXT NOT NULL, digest TEXT NOT NULL, PRIMARY KEY(unit,snapshot,kind,digest), FOREIGN KEY(unit,snapshot) REFERENCES snapshots(unit,id));
CREATE INDEX reusable ON refs(unit,kind,digest);
CREATE TABLE holds(id TEXT NOT NULL, unit TEXT NOT NULL, snapshot TEXT NOT NULL, path TEXT NOT NULL, device TEXT NOT NULL REFERENCES devices(id), created TEXT NOT NULL, released INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(unit,id), FOREIGN KEY(unit,snapshot) REFERENCES snapshots(unit,id));
` + schemaExternal + schemaProjects + schemaMerge + schemaTimeline + schemaConsole + schemaLibraryDelete
const schemaExternal = `
CREATE TABLE IF NOT EXISTS external_roots(unit TEXT PRIMARY KEY REFERENCES units(id), path TEXT NOT NULL, identity TEXT NOT NULL);
PRAGMA user_version=2;
`

type Store struct {
	archiveSlots    chan struct{}
	archiveUse      sync.RWMutex
	objectUse       sync.RWMutex
	durableDirs     sync.Map // synced object-kind and shard directories, bounded by 3 * 257
	batchDiskSlots  chan struct{}
	db              *sql.DB
	dir             string
	marker          string
	externalSlots   chan struct{}
	retentionCancel context.CancelFunc
	retentionDone   chan struct{}
}

func sqlite(dir string, create bool) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, "state", "meta.db")}
	q := u.Query()
	if !create {
		q.Set("mode", "rw")
	}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e == nil {
		db.SetMaxOpenConns(8)
	}
	return db, e
}
func Init(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("dataDir must be absolute")
	}
	if _, e := os.Stat(filepath.Join(dir, "store-id")); e == nil {
		s, e := Open(dir)
		if e == nil {
			return s.Close()
		}
		return e
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if names, e := os.ReadDir(dir); e != nil {
		return e
	} else if len(names) > 0 {
		return errors.New("initialization requires an empty dedicated directory")
	}
	for _, name := range []string{"state", "objects", "incoming"} {
		if e := os.Mkdir(filepath.Join(dir, name), 0700); e != nil {
			return e
		}
	}
	db, e := sqlite(dir, true)
	if e != nil {
		return e
	}
	defer db.Close()
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(schema); e != nil {
		return e
	}
	id := newID()
	if _, e = tx.Exec("INSERT INTO meta VALUES(?)", id); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if e = writeSynced(filepath.Join(dir, "store-id"), []byte(id+"\n")); e != nil {
		return e
	}
	return syncDir(dir)
}
func writeSynced(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	return e
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func Open(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("dataDir must be absolute")
	}
	b, e := os.ReadFile(filepath.Join(dir, "store-id"))
	if e != nil {
		return nil, e
	}
	id := strings.TrimSpace(string(b))
	if !validID.MatchString(id) {
		return nil, errors.New("invalid volume marker")
	}
	db, e := sqlite(dir, false)
	if e != nil {
		return nil, e
	}
	var version int
	var actual string
	if e = db.QueryRow("PRAGMA user_version").Scan(&version); e == nil && (version < 1 || version > 8) {
		e = fmt.Errorf("unsupported schema version %d", version)
	}
	if e == nil {
		e = db.QueryRow("SELECT storeid FROM meta").Scan(&actual)
	}
	if e != nil || actual != id {
		db.Close()
		return nil, fmt.Errorf("store identity/schema mismatch: %v", e)
	}
	if version < 8 {
		tx, err := db.Begin()
		if err != nil {
			db.Close()
			return nil, err
		}
		migration := schemaLibraryDelete
		if version < 7 {
			migration = schemaExternal + schemaProjects + schemaMerge + schemaTimeline + schemaConsole + migration
		}
		if _, err = tx.Exec(migration); err != nil {
			tx.Rollback()
			db.Close()
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{batchDiskSlots: make(chan struct{}, 4), db: db, dir: dir, marker: id, externalSlots: make(chan struct{}, 2), archiveSlots: make(chan struct{}, 2)}
	s.startRetention()
	return s, nil
}
func (s *Store) Close() error {
	if s.retentionCancel != nil {
		s.retentionCancel()
		<-s.retentionDone
	}
	return s.db.Close()
}
func (s *Store) volumeOK() bool {
	b, e := os.ReadFile(filepath.Join(s.dir, "store-id"))
	return e == nil && strings.TrimSpace(string(b)) == s.marker
}
func (s *Store) AddUnit(name string) (string, error) {
	if strings.TrimSpace(name) == "" || len([]rune(name)) > 255 {
		return "", errors.New("invalid unit name")
	}
	id := newID()
	_, e := s.db.Exec("INSERT INTO units(id,name) VALUES(?,?)", id, name)
	return id, e
}
func roleOK(role string) bool { return role == "reader" || role == "sync" }
func (s *Store) Issue(unit, role string) (string, error) {
	if !roleOK(role) {
		return "", errors.New("role must be reader or sync")
	}
	if role == "sync" {
		if x, e := s.external(unit); e != nil {
			return "", e
		} else if x != nil {
			return "", errors.New("external units are read-only")
		}
	}
	v := secret()
	_, e := s.db.Exec("INSERT INTO codes VALUES(?,?,?,?)", hash([]byte(v)), unit, role, time.Now().Add(10*time.Minute).Unix())
	return v, e
}
func (s *Store) Grant(unit, device, role string) error {
	if !roleOK(role) {
		return errors.New("invalid role")
	}
	if role == "sync" {
		if x, e := s.external(unit); e != nil {
			return e
		} else if x != nil {
			return errors.New("external units are read-only")
		}
	}
	r, e := s.db.Exec("INSERT INTO grants SELECT ?,id,? FROM devices WHERE id=? AND revoked=0 AND (maximum_role='sync' OR ?='reader') ON CONFLICT(unit,device) DO UPDATE SET role=excluded.role", unit, role, device, role)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return errors.New("device missing, revoked, or role exceeds enrollment")
	}
	return nil
}
func (s *Store) Revoke(id string) error {
	res, e := s.db.Exec("UPDATE devices SET revoked=1 WHERE id=?", id)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) pair(ctx context.Context, code, device, name, token, requiredRole string) error {
	return s.pairDevice(ctx, code, device, name, token, requiredRole, 0)
}
func (s *Store) pairDevice(ctx context.Context, code, device, name, token, requiredRole string, webExpires int64) error {
	b, e := base64.RawURLEncoding.Strict().DecodeString(token)
	if e != nil || len(b) != 32 || !validID.MatchString(device) || len(code) > 128 || len(code) < 20 || name == "" || len([]rune(name)) > 100 {
		return bad("invalid enrollment parameters")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var unit, role string
	e = tx.QueryRowContext(ctx, "DELETE FROM codes WHERE digest=? AND expires>? RETURNING unit,role", hash([]byte(code)), time.Now().Unix()).Scan(&unit, &role)
	if errors.Is(e, sql.ErrNoRows) {
		return unauth()
	}
	if e != nil {
		return e
	}
	if requiredRole != "" && role != requiredRole {
		return forbidden()
	}
	var prior int
	e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM devices WHERE id=? OR verifier=?", device, hash([]byte(token))).Scan(&prior)
	if e != nil {
		return e
	}
	if prior != 0 {
		return conflict("device already enrolled")
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO devices(id,name,verifier,maximum_role) VALUES(?,?,?,?)", device, name, hash([]byte(token)), role); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO grants VALUES(?,?,?)", unit, device, role); e != nil {
		return e
	}
	if webExpires > 0 {
		if _, e = tx.ExecContext(ctx, "INSERT INTO console_sessions VALUES(?,?)", device, webExpires); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func readConfig(path string) (Config, error) {
	var c Config
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	if c.FormatVersion != 1 || !filepath.IsAbs(c.DataDir) {
		return c, errors.New("formatVersion=1 and absolute dataDir required")
	}
	return c, nil
}
