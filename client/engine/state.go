package engine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const ClientName = "心狐同步"

var idPattern = regexp.MustCompile("^[0-7][0-9A-HJKMNP-TV-Z]{25}$")

func validID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid ULID: %q", id)
	}
	return nil
}
func newID() string {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(time.Now().UnixMilli())<<16)
	rand.Read(raw[6:])
	n := new(big.Int).SetBytes(raw[:])
	base := big.NewInt(32)
	digit := new(big.Int)
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		n.QuoRem(n, base, digit)
		out[i] = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"[digit.Int64()]
	}
	return string(out)
}

type Store struct {
	Home string
	db   *sql.DB
}

func clientHome(home string) (string, error) {
	if home == "" {
		home = os.Getenv("KDRIVE_HOME")
	}
	if home == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(base, "XinhuSync")
	}
	return filepath.Abs(home)
}

func Open(home string) (*Store, error) {
	home, err := clientHome(home)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "client.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=10000;
 PRAGMA journal_mode=WAL;
 PRAGMA synchronous=FULL;
 PRAGMA foreign_keys=ON;
 CREATE TABLE IF NOT EXISTS local_cleanup_plans(scope TEXT PRIMARY KEY,token TEXT NOT NULL UNIQUE,plan BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS local_cleanup(token TEXT NOT NULL,kind TEXT NOT NULL,key TEXT NOT NULL,digest TEXT NOT NULL,size INTEGER NOT NULL,PRIMARY KEY(token,kind,key,digest));
 CREATE TABLE IF NOT EXISTS credential_file(device TEXT PRIMARY KEY,path TEXT NOT NULL,digest TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS cached_files(unit TEXT NOT NULL,snapshot TEXT NOT NULL,path TEXT NOT NULL,state TEXT NOT NULL,digest TEXT NOT NULL,size INTEGER NOT NULL,hold TEXT NOT NULL UNIQUE,PRIMARY KEY(unit,snapshot,path));
 CREATE TABLE IF NOT EXISTS git_status_cache(unit TEXT PRIMARY KEY, checked INTEGER NOT NULL, result BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS service_identity(id INTEGER PRIMARY KEY CHECK(id=1), pin TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS codex_file_cache(source TEXT NOT NULL,path TEXT NOT NULL,signature TEXT NOT NULL,digest TEXT NOT NULL,PRIMARY KEY(source,path));
 CREATE TABLE IF NOT EXISTS traffic(id TEXT PRIMARY KEY, snapshot TEXT NOT NULL, at INTEGER NOT NULL, charged INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS account (id INTEGER PRIMARY KEY CHECK(id=1), url TEXT NOT NULL, ca BLOB, device TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS bindings ` + bindingColumns + `;
 CREATE TABLE IF NOT EXISTS schedules(unit TEXT PRIMARY KEY REFERENCES bindings(unit),policy TEXT NOT NULL,minutes INTEGER NOT NULL,last_scan INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS codex_preset(id INTEGER PRIMARY KEY CHECK(id=1),unit TEXT NOT NULL REFERENCES bindings(unit),scope TEXT NOT NULL,enabled INTEGER NOT NULL,last_error TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS reassociations(path TEXT PRIMARY KEY,previous TEXT NOT NULL,target TEXT NOT NULL,confirmed TEXT NOT NULL,generation TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS checkout_journal(unit TEXT NOT NULL,path TEXT PRIMARY KEY,prior TEXT NOT NULL,marker TEXT NOT NULL,local_root TEXT NOT NULL,record BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS project_proofs(unit TEXT PRIMARY KEY REFERENCES bindings(unit),base TEXT NOT NULL,expected TEXT NOT NULL,observed TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS checkout_base(unit TEXT PRIMARY KEY REFERENCES bindings(unit),snapshot TEXT NOT NULL,local_root TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS git_sources(unit TEXT PRIMARY KEY REFERENCES bindings(unit));
 CREATE TABLE IF NOT EXISTS ignored_selection(unit TEXT PRIMARY KEY REFERENCES bindings(unit), paths TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS binding_roots(unit TEXT PRIMARY KEY REFERENCES bindings(unit),identity TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS snapshots (id TEXT PRIMARY KEY, unit TEXT NOT NULL REFERENCES bindings(unit), parent TEXT, root TEXT NOT NULL, label TEXT NOT NULL, captured TEXT NOT NULL, request TEXT NOT NULL, state TEXT NOT NULL, receipt BLOB);
 CREATE INDEX IF NOT EXISTS queue_order ON snapshots(unit,captured,id);
 CREATE TABLE IF NOT EXISTS snapshot_objects (snapshot TEXT NOT NULL REFERENCES snapshots(id),kind TEXT NOT NULL,digest TEXT NOT NULL,size INTEGER NOT NULL,PRIMARY KEY(snapshot,kind,digest));
 CREATE INDEX IF NOT EXISTS spool_references ON snapshot_objects(kind,digest,snapshot);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = migrateBindings(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{home, db}, nil
}
func (s *Store) Close() error { return s.db.Close() }

const bindingsQuery = "SELECT unit,path,profile,base,generation,EXISTS(SELECT 1 FROM project_proofs p WHERE p.unit=bindings.unit AND p.expected<>p.observed AND NOT EXISTS(SELECT 1 FROM snapshots z WHERE z.unit=p.unit AND z.parent=p.base AND z.state='published')) FROM bindings WHERE path IS NOT NULL"

type Binding struct {
	generation  string
	ProtectHead bool    `json:"protectHead,omitempty"`
	Unit        string  `json:"unitId"`
	Path        string  `json:"path"`
	Profile     string  `json:"profile"`
	Base        *string `json:"baseSnapshotId"`
}

func (s *Store) binding(ctx context.Context, unit string) (Binding, error) {
	var b Binding
	if err := validID(unit); err != nil {
		return b, err
	}
	err := s.db.QueryRowContext(ctx, bindingsQuery+" AND unit=?", unit).Scan(&b.Unit, &b.Path, &b.Profile, &b.Base, &b.generation, &b.ProtectHead)
	return b, err
}
func (s *Store) Bindings(ctx context.Context) ([]Binding, error) {
	rows, e := s.db.QueryContext(ctx, bindingsQuery+" ORDER BY unit")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Binding{}
	for rows.Next() {
		var b Binding
		if e = rows.Scan(&b.Unit, &b.Path, &b.Profile, &b.Base, &b.generation, &b.ProtectHead); e != nil {
			return nil, e
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func inside(a, b string) bool {
	r, e := filepath.Rel(a, b)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
func checkCredentialPath(path string) error {
	token := os.Getenv("KDRIVE_TOKEN_FILE")
	if token == "" {
		return nil
	}
	p, err := filepath.Abs(token)
	if err != nil {
		return err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("检查凭据位置: %w", err)
	}
	if inside(path, p) {
		return errors.New("设备凭据不可位于同步源中")
	}
	return nil
}
func boundDirectory(path, profile string) string {
	if profile == "file" {
		return filepath.Dir(path)
	}
	return path
}
func directoryMarkerAt(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return directoryMarker(f)
}
func (s *Store) Bind(ctx context.Context, unit, path, profile string, preview bool, expectedBase string) (Binding, error) {
	if profile != "file" && profile != "project" {
		return Binding{}, errors.New("请选择普通文件或项目来源")
	}
	return s.bindSource(ctx, unit, path, profile, preview, expectedBase)
}

func (s *Store) bindSource(ctx context.Context, unit, path, profile string, preview bool, expectedBase string) (Binding, error) {
	return s.bindSourceWithPolicy(ctx, unit, path, profile, preview, expectedBase, nil)
}

func (s *Store) bindSourceWithPolicy(ctx context.Context, unit, path, profile string, preview bool, expectedBase string, requested *IgnoredPolicy) (Binding, error) {
	activity, lockErr := s.localLock(false)
	if lockErr != nil {
		return Binding{}, lockErr
	}
	defer activity.Close()
	initial := defaultIgnored()
	if requested != nil {
		initial = *requested
	}
	b := Binding{Unit: unit, Profile: profile}
	if err := validID(unit); err != nil {
		return b, err
	}
	if profile != "file" && profile != "project" && profile != "codex" {
		return b, fmt.Errorf("本版支持 file、project")
	}
	p, e := filepath.Abs(path)
	if e != nil {
		return b, e
	}
	var historyRoot os.FileInfo
	if profile == "codex" {
		historyRoot, e = os.Lstat(p)
		if e != nil {
			return b, e
		}
		if !historyRoot.IsDir() || codexReparse(historyRoot) {
			return b, errors.New("请选择实际的 Codex 历史目录")
		}
	}
	p, e = filepath.EvalSymlinks(p)
	if e != nil {
		return b, e
	}
	info, e := os.Stat(p)
	if e != nil {
		return b, e
	}
	if historyRoot != nil && !os.SameFile(historyRoot, info) {
		return b, errors.New("历史根目录已发生变化，请重新选择")
	}
	if (profile != "file" && !info.IsDir()) || (profile == "file" && !info.Mode().IsRegular()) {
		return b, errors.New("路径与 profile 不匹配")
	}
	if inside(p, s.Home) || inside(s.Home, p) {
		return b, errors.New("同步目录和客户端状态目录不可重叠")
	}
	if e = checkCredentialPath(p); e != nil {
		return b, e
	}
	marker, e := directoryMarkerAt(boundDirectory(p, profile))
	if e != nil {
		return b, e
	}
	b.Path = p
	c, e := s.Remote(ctx)
	if e != nil {
		return b, e
	}
	var units []unitInfo
	if e = c.unitList(ctx, &units); e != nil {
		return b, e
	}
	found := false
	shared := false
	for _, u := range units {
		if u.ID == unit && u.Kind == "managed" && u.Role == "sync" {
			found = true
			b.Base = u.Head
			shared = u.ScopeID != ""
			break
		}
	}
	if !found {
		return b, errors.New("缺少该托管资料库的同步授权")
	}
	var observed, expected string
	if shared && b.Base != nil && profile != "codex" {
		var exists bool
		if e = s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM bindings WHERE unit=? AND path IS NOT NULL)", unit).Scan(&exists); e != nil {
			return b, e
		}
		if !exists && (preview || *b.Base == expectedBase) {
			var record struct {
				Digest string `json:"contentDigest"`
			}
			if e = c.call(ctx, "GET", "/units/"+unit+"/snapshots/"+*b.Base, fingerprintQuery, nil, &record); e != nil {
				return b, e
			}
			expected = record.Digest
			if len(expected) != 64 {
				return b, errors.New("服务端不支持共享项目基线校验，请升级梦枢天罗")
			}
			if observed, e = baselineFingerprint(ctx, b, initial); e != nil {
				return b, e
			}
			b.ProtectHead = observed != expected
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return b, e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, bindingsQuery)
	if e != nil {
		return b, e
	}
	for rows.Next() {
		var old Binding
		if e = rows.Scan(&old.Unit, &old.Path, &old.Profile, &old.Base, &old.generation, &old.ProtectHead); e != nil {
			rows.Close()
			return b, e
		}
		if old.Unit == unit && old.Path == p && old.Profile == profile {
			rows.Close()
			var previous string
			err := tx.QueryRowContext(ctx, "SELECT identity FROM binding_roots WHERE unit=?", unit).Scan(&previous)
			if errors.Is(err, sql.ErrNoRows) {
				if preview {
					return old, nil
				}
				if _, err = tx.ExecContext(ctx, "INSERT INTO binding_roots VALUES(?,?)", unit, marker); err != nil {
					return old, err
				}
				return old, tx.Commit()
			}
			if err != nil {
				return old, err
			}
			if previous != marker {
				return old, errors.New("来源目录已更换或卸载，请恢复原目录")
			}
			return old, nil
		}
		if old.Unit == unit || inside(old.Path, p) || inside(p, old.Path) {
			rows.Close()
			return b, errors.New("目录或单元已经绑定")
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return b, e
	}
	if !preview && ((b.Base != nil && *b.Base != expectedBase) || (b.Base == nil && expectedBase != "")) {
		return b, errors.New("远端基线与 --base 不符。请用 --preview 查看当前版本，先恢复后绑定，或明确指定要接续的基线")
	}
	if preview {
		return b, nil
	}
	b.generation = newID()
	_, e = tx.ExecContext(ctx, "INSERT INTO bindings(unit,path,profile,base,generation) VALUES(?,?,?,?,?) ON CONFLICT(unit) DO UPDATE SET path=excluded.path,profile=excluded.profile,base=excluded.base,generation=excluded.generation WHERE bindings.path IS NULL", b.Unit, b.Path, b.Profile, b.Base, b.generation)
	if e != nil {
		return b, e
	}
	if e = clearSourceSettings(ctx, tx, b.Unit); e != nil {
		return b, e
	}
	if requested != nil {
		if e = writeIgnored(ctx, tx, unit, initial); e != nil {
			return b, e
		}
	}
	if expected != "" {
		if _, e = tx.ExecContext(ctx, "INSERT INTO project_proofs VALUES(?,?,?,?)", unit, *b.Base, expected, observed); e != nil {
			return b, e
		}
	}
	if profile == "project" {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			if _, e = tx.ExecContext(ctx, "INSERT INTO git_sources(unit) VALUES(?)", unit); e != nil {
				return b, e
			}
		} else if !os.IsNotExist(err) {
			return b, err
		}
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO binding_roots VALUES(?,?)", unit, marker); e != nil {
		return b, e
	}
	return b, tx.Commit()
}
func syncDirectory(p string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
