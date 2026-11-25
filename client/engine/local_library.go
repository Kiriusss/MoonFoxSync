package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

type LocalRemovalPlan struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Bindings  int    `json:"bindings"`
	Snapshots int    `json:"snapshots"`
	Queued    int    `json:"queued"`
	Cached    int    `json:"cached"`
	NASOnly   int    `json:"nasOnly"`
	Token     string `json:"token"`
}

// Readers may capture, transfer and cache concurrently. Cleanup exclusively owns
// the app's files until database references and physical objects agree again.
func (s *Store) localLock(exclusive bool) (*os.File, error) {
	if !exclusive {
		s.scopedMaintenanceReady()
	}
	f, err := lockFile(filepath.Join(s.Home, "local-maintenance.lock"), exclusive)
	if err != nil {
		if lockBusy(err) {
			return nil, errors.New("本机正在执行同步或清理，任务结束后请重试")
		}
		return nil, fmt.Errorf("读取本机维护锁失败: %w", err)
	}
	return f, nil
}

// OpenRemovalPreview reads a current client database through WAL without doing
// the normal startup writes. Older databases still take the migration path.
func OpenRemovalPreview(home string) (*Store, error) {
	dir, err := clientHome(home)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Open(home)
	}
	if err != nil {
		return nil, err
	}
	file := filepath.Join(dir, "client.sqlite")
	if _, err = os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		return Open(home)
	} else if err != nil {
		return nil, err
	}
	escaped := (&url.URL{Path: filepath.ToSlash(file)}).EscapedPath()
	db, err := sql.Open("sqlite", "file:"+escaped+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var ready bool
	err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='local_cleanup_plans')
        AND EXISTS(SELECT 1 FROM pragma_table_info('bindings') WHERE name='generation')`).Scan(&ready)
	if err != nil || !ready {
		db.Close()
		if err != nil {
			return nil, err
		}
		return Open(home)
	}
	return &Store{dir, db}, nil
}

func (s *Store) LocalRemovalPlan(ctx context.Context, unit string) (LocalRemovalPlan, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalRemovalPlan{}, err
	}
	defer tx.Rollback()
	if pending, err := pendingLocalPlan(ctx, tx, unit); err != nil {
		return LocalRemovalPlan{}, err
	} else if pending != nil {
		return *pending, nil
	}
	return s.localRemovalPlan(ctx, tx, unit)
}

func pendingLocalPlan(ctx context.Context, q policyReader, scope string) (*LocalRemovalPlan, error) {
	var raw []byte
	err := q.QueryRowContext(ctx, "SELECT plan FROM local_cleanup_plans WHERE scope=?", scope).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p LocalRemovalPlan
	if err = json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) localRemovalPlan(ctx context.Context, tx *sql.Tx, unit string) (LocalRemovalPlan, error) {
	var p LocalRemovalPlan
	if unit != "" {
		if err := validID(unit); err != nil {
			return p, err
		}
		p.Name = unit
	}
	h := sha256.New()
	fmt.Fprintf(h, "%q|%q|", s.Home, unit)
	var device string
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT device FROM account WHERE id=1),'')").Scan(&device); err != nil {
		return p, err
	}
	fmt.Fprintf(h, "%q|", device)
	rows, err := tx.QueryContext(ctx, `SELECT 'binding',unit,COALESCE(path,''),generation FROM bindings WHERE (?1='' OR unit=?1)
		UNION ALL SELECT 'snapshot',id,state,root FROM snapshots WHERE (?1='' OR unit=?1)
		UNION ALL SELECT 'cache',snapshot,state,path||char(0)||unit||char(0)||digest FROM cached_files WHERE (?1='' OR unit=?1)
		ORDER BY 1,2,3,4`, unit)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id, value, detail string
		if err = rows.Scan(&kind, &id, &value, &detail); err != nil {
			return p, err
		}
		fmt.Fprintf(h, "%q%q%q%q", kind, id, value, detail)
		switch kind {
		case "binding":
			p.Bindings++
			if unit != "" {
				p.Path = value
				if value != "" {
					p.Name = filepath.Base(value)
				}
			}
		case "snapshot":
			p.Snapshots++
			if value == "queued" {
				p.Queued++
			}
		case "cache":
			p.Cached++
			if value == "nas_only" || value == "evicting" {
				p.NASOnly++
			}
		}
	}
	if err = rows.Err(); err != nil {
		return p, err
	}
	if unit != "" && p.Bindings+p.Snapshots+p.Cached == 0 {
		return p, errors.New("此项目没有本机绑定、缓存或快照")
	}
	p.Token = hex.EncodeToString(h.Sum(nil))
	return p, nil
}

type cleanupItem struct {
	kind, key, digest string
	size              int64
}

func checkOwnedFile(path, digest string, expected int64) error {
	hash, n, err := fileHash(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if hash != digest || expected >= 0 && n != expected {
		return fmt.Errorf("本机文件已被修改，请先保存或移出后重试：%s", path)
	}
	return nil
}

func ownedCacheItems(ctx context.Context, s *Store, tx *sql.Tx, unit string) ([]cleanupItem, error) {
	dir, err := s.cacheDir()
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+cacheCols+" FROM cached_files WHERE (?1='' OR unit=?1)", unit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []cleanupItem
	for rows.Next() {
		entry, err := scanCache(rows)
		if err != nil {
			return nil, err
		}
		key := cacheName(entry.Unit, entry.Snapshot, entry.Path)
		for _, suffix := range []string{"", ".evicting"} {
			name := key + suffix
			path := filepath.Join(dir, name)
			if _, err = os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, err
			}
			if err = checkOwnedFile(path, entry.SHA, entry.Size); err != nil {
				return nil, err
			}
			result = append(result, cleanupItem{"download", name, entry.SHA, entry.Size})
		}
	}
	return result, rows.Err()
}

func (s *Store) RemoveLocal(ctx context.Context, unit, token string) (LocalRemovalPlan, error) {
	var p LocalRemovalPlan
	if unit != "" {
		if err := validID(unit); err != nil {
			return p, err
		}
	}
	if len(token) != 64 {
		return p, errors.New("请先预览并确认本机删除范围")
	}
	// An old binary may still hold a pre-upgrade, unscoped operation.
	// Establish the scoped protocol only across a point with no such holders.
	scoped := unit != "" && s.scopedMaintenanceReady()
	lock, err := s.localLock(!scoped)
	if err != nil {
		if unit != "" && !scoped {
			return p, fmt.Errorf("正在等待已有任务退出旧版维护锁，请稍后重试：%w", err)
		}
		return p, err
	}
	defer lock.Close()
	if unit != "" {
		project, err := s.projectLock(unit, true)
		if err != nil {
			return p, err
		}
		defer project.Close()
	}
	// SQLite commits the reference removal and its cleanup journal together.
	// A retry can finish an interrupted physical cleanup without a surviving binding.
	if err := s.guardWorkspace(ctx, unit); err != nil {
		return p, err
	}
	previous, err := pendingLocalPlan(ctx, s.db, unit)
	if err != nil {
		return p, err
	}
	if previous != nil {
		if previous.Token != token {
			return p, errors.New("上次清理尚未完成，请重新打开预览重试")
		}
		return *previous, s.finishLocalCleanup(ctx, token, unit == "")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE bindings SET base=base WHERE 0"); err != nil {
		return p, err
	}
	p, err = s.localRemovalPlan(ctx, tx, unit)
	if err != nil {
		return p, err
	}
	if p.Token != token {
		return p, errors.New("预览后本机绑定、快照、队列或缓存已变化，请重新确认")
	}
	items, err := ownedCacheItems(ctx, s, tx, unit)
	if err != nil {
		return p, err
	}
	if unit == "" {
		var device string
		err = tx.QueryRowContext(ctx, "SELECT device FROM account WHERE id=1").Scan(&device)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return p, err
		}
		if device != "" {
			if err = validID(device); err != nil {
				return p, err
			}
			var path, digest string
			err = tx.QueryRowContext(ctx, "SELECT path,digest FROM credential_file WHERE device=?", device).Scan(&path, &digest)
			switch {
			case err == nil:
				if err = checkOwnedFile(path, digest, -1); err != nil {
					return p, err
				}
				items = append(items, cleanupItem{"token-file", path, digest, -1})
			case errors.Is(err, sql.ErrNoRows):
				items = append(items, cleanupItem{"system-credential", device, "", 0})
			default:
				return p, err
			}
		}
		// A capture interrupted before committing its snapshot may have left
		// unreferenced spool objects. Sweep the spool on a full reset.
		items = append(items, cleanupItem{"sweep", "spool", "", 0})
	} else {
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO deferred_spool_gc SELECT o.kind,o.digest FROM snapshot_objects o JOIN snapshots s ON s.id=o.snapshot WHERE s.unit=?1 AND NOT EXISTS(SELECT 1 FROM snapshot_objects a JOIN snapshots b ON a.snapshot=b.id WHERE a.kind=o.kind AND a.digest=o.digest AND b.unit<>?1)`, unit)
		if err != nil {
			return p, err
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	if unit == "" {
		// The confirmed reset also completes cleanup interrupted after a prior
		// single-project removal. Reopen the reset preview to retry any failure.
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO local_cleanup SELECT ?,kind,key,digest,size FROM local_cleanup WHERE token<>?", token, token); err != nil {
			return p, err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM local_cleanup WHERE token<>?", token); err != nil {
			return p, err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM local_cleanup_plans"); err != nil {
			return p, err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO local_cleanup_plans(scope,token,plan) VALUES(?,?,?)", unit, token, raw); err != nil {
		return p, err
	}
	for _, item := range items {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO local_cleanup VALUES(?,?,?,?,?)", token, item.kind, item.key, item.digest, item.size); err != nil {
			return p, err
		}
	}
	for _, query := range []string{
		"DELETE FROM traffic WHERE ?1='' OR snapshot IN(SELECT id FROM snapshots WHERE unit=?1)",
		"DELETE FROM snapshot_objects WHERE ?1='' OR snapshot IN(SELECT id FROM snapshots WHERE unit=?1)",
		"DELETE FROM tree_totals WHERE ?1='' OR NOT EXISTS(SELECT 1 FROM snapshot_objects o WHERE o.kind='tree' AND o.digest=tree_totals.digest)",
		"UPDATE transfer_settings SET uploads=4,downloads=4 WHERE ?1=''",
		"DELETE FROM snapshots WHERE ?1='' OR unit=?1",
		"DELETE FROM cached_files WHERE ?1='' OR unit=?1",
		"DELETE FROM codex_file_cache WHERE ?1='' OR source IN(SELECT r.identity FROM binding_roots r JOIN bindings b ON b.unit=r.unit WHERE b.unit=?1 AND b.profile='codex')",
		"DELETE FROM reassociations WHERE ?1='' OR previous=?1 OR target=?1",
	} {
		if _, err = tx.ExecContext(ctx, query, unit); err != nil {
			return p, err
		}
	}
	for _, table := range []string{"codex_preset", "schedules", "ignored_selection", "git_sources", "git_status_cache", "checkout_base", "checkout_journal", "project_proofs", "binding_roots", "bindings"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE ?1='' OR unit=?1", unit); err != nil {
			return p, err
		}
	}
	if unit == "" {
		if _, err = tx.ExecContext(ctx, "DELETE FROM credential_file; DELETE FROM service_identity; DELETE FROM account"); err != nil {
			return p, err
		}
	}
	if err = tx.Commit(); err != nil {
		return p, err
	}
	return p, s.finishLocalCleanup(ctx, token, unit == "")
}

// The queue contains only app-owned objects whose metadata is already removed.
// References can reappear after interruption, so each deletion checks again.
func (s *Store) finishLocalCleanup(ctx context.Context, token string, compact bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT kind,key,digest,size FROM local_cleanup WHERE token=? ORDER BY kind,key,digest", token)
	if err != nil {
		return err
	}
	var items []cleanupItem
	for rows.Next() {
		var item cleanupItem
		if err = rows.Scan(&item.kind, &item.key, &item.digest, &item.size); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range items {
		if err = s.removeCleanupItem(ctx, tx, item); err != nil {
			return fmt.Errorf("本机记录已移除，清理尚未完成，使用原确认码重试：%w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = s.collectDeferredSpool(ctx); err != nil {
		return fmt.Errorf("本机记录已移除，清理尚未完成，使用原确认码重试：%w", err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM local_cleanup WHERE token=?", token); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM local_cleanup_plans WHERE token=?", token); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if compact {
		_, err = s.db.ExecContext(ctx, "VACUUM; PRAGMA wal_checkpoint(TRUNCATE)")
	}
	return err
}

func (s *Store) removeCleanupItem(ctx context.Context, tx *sql.Tx, item cleanupItem) error {
	switch item.kind {
	case "system-credential":
		err := keyring.Delete("xinhu-sync", item.key)
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return err
	case "token-file", "download":
		path := item.key
		if item.kind == "download" {
			// A newly fetched record can own this name after a crash.
			rows, err := tx.QueryContext(ctx, "SELECT unit,snapshot,path FROM cached_files")
			if err != nil {
				return err
			}
			for rows.Next() {
				var u, v, p string
				if err = rows.Scan(&u, &v, &p); err != nil {
					rows.Close()
					return err
				}
				if cacheName(u, v, p) == strings.TrimSuffix(item.key, ".evicting") {
					rows.Close()
					return nil
				}
			}
			if err = rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			dir, err := s.cacheDir()
			if err != nil {
				return err
			}
			if filepath.Base(path) != path {
				return errors.New("缓存文件名无效")
			}
			path = filepath.Join(dir, path)
		}
		return removeOwnedFile(path, item.digest, item.size)
	case "sweep":
		return s.sweepSpool(ctx, tx)
	case "object":
		// Old interrupted journals also go through the capture-safe collector.
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO deferred_spool_gc VALUES(?,?)", item.key, item.digest)
		return err
	}
	return errors.New("不支持的本机清理项")
}

func removeOwnedFile(path, digest string, size int64) error {
	// An interrupted rename is resumed and modified bytes are always preserved.
	isolated := path + ".xinhu-removing"
	if _, err := os.Lstat(isolated); errors.Is(err, fs.ErrNotExist) {
		if _, err = os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		if err = checkOwnedFile(path, digest, size); err != nil {
			return err
		}
		if err = publishNew(path, isolated); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := checkOwnedFile(isolated, digest, size); err != nil {
		return err
	}
	return removeReadOnly(isolated)
}

func (s *Store) removeSpoolObject(ctx context.Context, tx *sql.Tx, kind, digest string) error {
	if !validObjectName(kind, digest) {
		return errors.New("本机对象路径无效")
	}
	var used bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM snapshot_objects WHERE kind=? AND digest=?)", kind, digest).Scan(&used); err != nil || used {
		return err
	}
	dir := filepath.Join(s.Home, "spool")
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("本机 spool 目录身份无效")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.Remove(filepath.Join(kind, digest[:2], digest))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func validObjectName(kind, digest string) bool {
	if kind != "file" && kind != "chunk" && kind != "tree" {
		return false
	}
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func (s *Store) sweepSpool(ctx context.Context, tx *sql.Tx) error {
	dir := filepath.Join(s.Home, "spool")
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) == 3 && validObjectName(parts[0], parts[2]) && parts[1] == parts[2][:2] {
			return s.removeSpoolObject(ctx, tx, parts[0], parts[2])
		}
		return nil
	})
}
