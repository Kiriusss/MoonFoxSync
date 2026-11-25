package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

const schemaLibraryDelete = `
 CREATE TABLE IF NOT EXISTS delete_managers(unit TEXT NOT NULL REFERENCES units(id),device TEXT NOT NULL REFERENCES devices(id),PRIMARY KEY(unit,device));
 CREATE TABLE IF NOT EXISTS removed_library_objects(kind TEXT NOT NULL,digest TEXT NOT NULL,PRIMARY KEY(kind,digest));
 CREATE INDEX IF NOT EXISTS all_library_references ON refs(kind,digest);
 CREATE INDEX IF NOT EXISTS active_library_offers ON offers(kind,digest);
 PRAGMA user_version=8;`

func (s *Store) setDeleteManager(unit, device string, allow bool) error {
	if !allow {
		_, err := s.db.Exec("DELETE FROM delete_managers WHERE unit=? AND device=?", unit, device)
		return err
	}
	if err := s.access(unit, device, true); err != nil {
		return err
	}
	_, err := s.db.Exec("INSERT OR IGNORE INTO delete_managers VALUES(?,?)", unit, device)
	return err
}

type libraryDeletePlan struct {
	Name      string `json:"name"`
	Units     int    `json:"units"`
	Snapshots int    `json:"snapshots"`
	Devices   int    `json:"devices"`
	Holds     int    `json:"holds"`
	Uploads   int    `json:"uploads"`
	Token     string `json:"token"`
}

// ids is a frozen JSON array. It stays valid after removing the projects mapping.
const inLibraries = " IN (SELECT value FROM json_each(?))"

func buildLibraryPlan(ctx context.Context, tx *sql.Tx, unit, dev string) (p libraryDeletePlan, ids string, err error) {
	var role string
	err = tx.QueryRowContext(ctx, `SELECT u.name,g.role FROM units u JOIN effective_grants g ON g.unit=u.id JOIN devices d ON d.id=g.device AND d.revoked=0 WHERE u.id=? AND g.device=?`, unit, dev).Scan(&p.Name, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return p, "", absent()
	}
	if err != nil {
		return p, "", err
	}
	var allowed bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM delete_managers WHERE device=? AND (unit=? OR unit=(SELECT scope FROM projects WHERE unit=?)))`, dev, unit, unit).Scan(&allowed)
	if err != nil {
		return p, "", err
	}
	if role != "sync" || !allowed {
		return p, "", &problem{403, "DELETE_PERMISSION_REQUIRED", "此设备没有该资料库的删除管理权限", "stop", nil}
	}
	err = tx.QueryRowContext(ctx, `SELECT json_group_array(id ORDER BY id),count(*) FROM units WHERE id=? OR id IN (SELECT unit FROM projects WHERE scope=?)`, unit, unit).Scan(&ids, &p.Units)
	if err != nil {
		return p, "", err
	}
	var writable int
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM effective_grants WHERE device=? AND role='sync' AND unit"+inLibraries, dev, ids).Scan(&writable)
	if err != nil {
		return p, "", err
	}
	if writable != p.Units {
		return p, "", forbidden()
	}
	var latest, tombstones int64
	err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(max(seq),0) FROM snapshots WHERE unit"+inLibraries, ids).Scan(&p.Snapshots, &latest)
	if err != nil {
		return p, "", err
	}
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM snapshot_tombstones WHERE unit"+inLibraries, ids).Scan(&tombstones)
	if err != nil {
		return p, "", err
	}
	var grants, holds, uploads string
	err = tx.QueryRowContext(ctx, "SELECT count(DISTINCT g.device),coalesce(group_concat(g.unit||g.device||g.role,',' ORDER BY g.unit,g.device),'') FROM effective_grants g JOIN devices d ON d.id=g.device AND d.revoked=0 WHERE g.unit"+inLibraries, ids).Scan(&p.Devices, &grants)
	if err != nil {
		return p, "", err
	}
	err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(group_concat(id,',' ORDER BY id),'') FROM holds WHERE released=0 AND unit"+inLibraries, ids).Scan(&p.Holds, &holds)
	if err != nil {
		return p, "", err
	}
	err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(group_concat(id,',' ORDER BY id),'') FROM uploads WHERE receipt IS NULL AND expires>? AND unit"+inLibraries, time.Now().Unix(), ids).Scan(&p.Uploads, &uploads)
	if err != nil {
		return p, "", err
	}
	raw, err := json.Marshal([]any{dev, unit, p, ids, latest, tombstones, grants, holds, uploads})
	p.Token = hash(raw)
	return p, ids, err
}

func (s *Store) deleteLibrary(w http.ResponseWriter, r *http.Request, dev string) error {
	unit := r.PathValue("unitId")
	var confirmed struct{ Name, Token string }
	if r.Method == "POST" {
		if err := decode(r, &confirmed, 4096); err != nil {
			return err
		}
		// Finish any write of content-addressed bytes before unlinking its metadata.
		s.objectUse.Lock()
		defer s.objectUse.Unlock()
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, ids, err := buildLibraryPlan(r.Context(), tx, unit, dev)
	if err != nil {
		return err
	}
	if r.Method == "GET" {
		return reply(w, 200, p)
	}
	if confirmed.Token != p.Token || confirmed.Name != p.Name {
		return conflict("名称或删除范围已变化，请重新预览并确认")
	}
	// Record only this library's candidates; physical cleanup rechecks ownership across the NAS.
	for _, q := range []string{
		"INSERT OR IGNORE INTO removed_library_objects SELECT kind,digest FROM refs WHERE unit" + inLibraries,
		"INSERT OR IGNORE INTO removed_library_objects SELECT o.kind,o.digest FROM offers o JOIN uploads u ON u.id=o.upload WHERE u.unit" + inLibraries,
		"DELETE FROM offers WHERE upload IN (SELECT id FROM uploads WHERE unit" + inLibraries + ")",
	} {
		if _, err = tx.ExecContext(r.Context(), q, ids); err != nil {
			return err
		}
	}
	for _, table := range []string{"uploads", "delete_receipts", "snapshot_tombstones", "timeline_bases", "holds", "refs", "snapshots", "restore_requests", "merge_requests", "history_policies", "project_origins", "projects", "delete_managers", "grants", "codes", "external_roots"} {
		if _, err = tx.ExecContext(r.Context(), "DELETE FROM "+table+" WHERE unit"+inLibraries, ids); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(r.Context(), "DELETE FROM units WHERE id"+inLibraries, ids); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, 200, fields{"deleted": true, "units": p.Units, "snapshots": p.Snapshots})
}

// Bound every batch so normal transfers can continue between cleanup transactions.
// The queue survives restarts. Shared objects and objects offered to any other upload stay available.
func (s *Store) collectLibraryObjects(ctx context.Context) (int, error) {
	// A stream may still need immutable chunks of a library deleted after the
	// download started. Defer physical cleanup without blocking new uploads.
	if !s.archiveUse.TryLock() {
		return 0, nil
	}
	defer s.archiveUse.Unlock()
	s.objectUse.Lock()
	defer s.objectUse.Unlock()
	if !s.volumeOK() {
		return 0, errors.New("NAS 数据卷不可用")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT kind,digest,EXISTS(SELECT 1 FROM refs r WHERE r.kind=c.kind AND r.digest=c.digest) OR EXISTS(SELECT 1 FROM offers o WHERE o.kind=c.kind AND o.digest=c.digest) FROM removed_library_objects c LIMIT 128`)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		ref    objectfmt.Ref
		shared bool
	}
	var items []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.ref.Kind, &c.ref.Digest, &c.shared); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, c := range items {
		if !goodRef(c.ref) {
			return 0, errors.New("无效的回收对象")
		}
		if !c.shared {
			if err = os.Remove(s.path(c.ref)); err != nil && !os.IsNotExist(err) {
				return 0, err
			}
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM removed_library_objects WHERE kind=? AND digest=?", c.ref.Kind, c.ref.Digest); err != nil {
			return 0, err
		}
	}
	return len(items), tx.Commit()
}

func (s *Store) runLibraryGC(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		n, err := s.collectLibraryObjects(ctx)
		delay := 15 * time.Second
		if err != nil && ctx.Err() == nil {
			log.Printf("资料库回收: %v", err)
		}
		if n == 128 {
			delay = 20 * time.Millisecond
		}
		timer.Reset(delay)
	}
}
