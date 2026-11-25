package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
	"unicode/utf8"
)

const schemaTimeline = `
CREATE TABLE IF NOT EXISTS history_policies(unit TEXT PRIMARY KEY REFERENCES units(id),mode TEXT NOT NULL,value INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS snapshot_tombstones(unit TEXT NOT NULL,snapshot TEXT NOT NULL,deleted TEXT NOT NULL,cause TEXT NOT NULL,PRIMARY KEY(unit,snapshot),FOREIGN KEY(unit,snapshot) REFERENCES snapshots(unit,id));
CREATE VIEW IF NOT EXISTS visible_snapshots AS SELECT s.* FROM snapshots s WHERE NOT EXISTS(SELECT 1 FROM snapshot_tombstones t WHERE t.unit=s.unit AND t.snapshot=s.id);
CREATE VIEW IF NOT EXISTS visible_refs AS SELECT r.* FROM refs r WHERE NOT EXISTS(SELECT 1 FROM snapshot_tombstones t WHERE t.unit=r.unit AND t.snapshot=r.snapshot);
CREATE INDEX IF NOT EXISTS snapshot_unit_seq ON snapshots(unit,seq);
CREATE INDEX IF NOT EXISTS active_snapshot_holds ON holds(unit,snapshot) WHERE released=0;
CREATE TABLE IF NOT EXISTS delete_receipts(unit TEXT NOT NULL, snapshot TEXT NOT NULL,device TEXT NOT NULL REFERENCES devices(id),head TEXT NOT NULL,token TEXT NOT NULL,receipt BLOB NOT NULL,PRIMARY KEY(unit,snapshot,device),FOREIGN KEY(unit,snapshot) REFERENCES snapshots(unit,id));
CREATE TABLE IF NOT EXISTS restore_requests(unit TEXT NOT NULL REFERENCES units(id),device TEXT NOT NULL REFERENCES devices(id),request TEXT NOT NULL,bodyhash TEXT NOT NULL,receipt BLOB NOT NULL,PRIMARY KEY(unit,device,request));
-- Old offers did not record whether ready meant supplied or deduplicated. Keep
-- private pending uploads whose objects were never referenced by a snapshot.
UPDATE offers SET ready=2 WHERE ready=1 AND NOT EXISTS (
 SELECT 1 FROM uploads u JOIN refs r ON r.unit=u.unit WHERE u.id=offers.upload AND r.kind=offers.kind AND r.digest=offers.digest);
PRAGMA user_version=6;
` + schemaHistoryRebuild

type historyPolicy struct {
	Mode  string `json:"mode"`
	Value int    `json:"value"`
}

func (p historyPolicy) valid() bool {
	return p.Mode == "all" && p.Value == 0 || p.Mode == "latest" && p.Value >= 1 && p.Value <= 10000 || p.Mode == "days" && p.Value >= 1 && p.Value <= 3650
}

// Traverse both immutable parent edges, including hidden ancestors. All
// unreachable forks remain protected. A hold can cover just one member but
// pins its entire snapshot. Protected extra points do not use a latest-N slot.
const retentionRows = `WITH RECURSIVE ancestors(id) AS (
 SELECT head FROM units WHERE id=:unit AND head IS NOT NULL
 UNION SELECT json_extract(s.record,'$.parentSnapshotId') FROM snapshots s JOIN ancestors a ON a.id=s.id
 WHERE s.unit=:unit AND json_extract(s.record,'$.parentSnapshotId') IS NOT NULL
 UNION SELECT json_extract(s.record,'$.mergeParentSnapshotId') FROM snapshots s JOIN ancestors a ON a.id=s.id
 WHERE s.unit=:unit AND json_extract(s.record,'$.mergeParentSnapshotId') IS NOT NULL
), points AS (
 SELECT s.id,s.seq,s.id=u.head AS ishead,unixepoch(json_extract(s.record,'$.committedAt')) AS committed,
 (s.id=u.head OR EXISTS(SELECT 1 FROM holds h WHERE h.unit=s.unit AND h.snapshot=s.id AND h.released=0)
 OR (json_extract(s.record,'$.outcome')='fork' AND s.id NOT IN (SELECT id FROM ancestors))) AS protected
 FROM visible_snapshots s JOIN units u ON u.id=s.unit WHERE s.unit=:unit
), ranked AS (
 SELECT *,sum(CASE WHEN ishead OR NOT protected THEN 1 ELSE 0 END) OVER (ORDER BY seq DESC) AS pos FROM points
), decision AS (
 SELECT id,protected, NOT protected AND ((:mode='latest' AND pos>:value) OR (:mode='days' AND committed<:cutoff)) AS remove FROM ranked
) `

func policyArgs(unit string, p historyPolicy, now time.Time) []any {
	return []any{sql.Named("unit", unit), sql.Named("mode", p.Mode), sql.Named("value", p.Value), sql.Named("cutoff", now.Add(-time.Duration(p.Value)*24*time.Hour).Unix())}
}
func getPolicy(ctx context.Context, q rowQuery, unit string) (historyPolicy, error) {
	p := historyPolicy{Mode: "all"}
	err := q.QueryRowContext(ctx, "SELECT mode,value FROM history_policies WHERE unit=?", unit).Scan(&p.Mode, &p.Value)
	if errors.Is(err, sql.ErrNoRows) {
		return historyPolicy{Mode: "all"}, nil
	}
	return p, err
}
func (s *Store) applyPolicy(ctx context.Context, tx *sql.Tx, unit string, now time.Time) error {
	p, err := getPolicy(ctx, tx, unit)
	if err != nil || p.Mode == "all" {
		return err
	}
	rows, err := tx.QueryContext(ctx, retentionRows+"SELECT id FROM decision WHERE remove ORDER BY id", policyArgs(unit, p, now)...)
	if err != nil {
		return err
	}
	var removed []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		removed = append(removed, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(removed) == 0 {
		return err
	}
	if _, err = s.rebuildHistory(ctx, tx, unit, removed); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO snapshot_tombstones VALUES(?,?,?,'policy')")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range removed {
		if _, err = stmt.ExecContext(ctx, unit, id, stamp(now)); err != nil {
			return err
		}
	}
	return nil
}

// Recheck role, revocation and current head under the same SQLite write lock as
// each mutation. External roots have no timeline.
func timelineHead(ctx context.Context, tx *sql.Tx, unit, device string, write bool) (string, error) {
	var role, head string
	err := tx.QueryRowContext(ctx, `SELECT g.role,COALESCE(u.head,'') FROM units u JOIN effective_grants g ON u.id=g.unit JOIN devices d ON d.id=g.device WHERE u.id=? AND g.device=? AND d.revoked=0 AND NOT EXISTS(SELECT 1 FROM external_roots x WHERE x.unit=u.id)`, unit, device).Scan(&role, &head)
	if err != nil {
		return "", err
	}
	if write && role != "sync" {
		return "", forbidden()
	}
	return head, nil
}
func (s *Store) historyPolicy(w http.ResponseWriter, r *http.Request, dev string) error {
	unit, ctx := r.PathValue("unitId"), r.Context()
	write := r.Method == http.MethodPut || r.Method == http.MethodPost
	var input struct {
		Mode         string  `json:"mode"`
		Value        *int    `json:"value"`
		ExpectedHead *string `json:"expectedHead,omitempty"`
	}
	var policy historyPolicy
	if write {
		if err := decode(r, &input, 2048); err != nil {
			return err
		}
		if input.Value == nil {
			return bad("retention value is required")
		}
		policy = historyPolicy{input.Mode, *input.Value}
		if !policy.valid() {
			return bad("invalid retention mode or value")
		}
		if input.ExpectedHead != nil && *input.ExpectedHead != "" && !validID.MatchString(*input.ExpectedHead) {
			return bad("invalid expected head")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	head, err := timelineHead(ctx, tx, unit, dev, write)
	if err != nil {
		return err
	}
	if input.ExpectedHead != nil && *input.ExpectedHead != head {
		return conflict("主线已变化，请刷新历史策略预览")
	}
	if !write {
		p, err := getPolicy(ctx, tx, unit)
		if err != nil {
			return err
		}
		return reply(w, 200, p)
	}
	now := time.Now().UTC()
	if r.Method == http.MethodPost {
		var removed, protected int
		if err = tx.QueryRowContext(ctx, retentionRows+"SELECT coalesce(sum(remove),0),coalesce(sum(protected),0) FROM decision", policyArgs(unit, policy, now)...).Scan(&removed, &protected); err != nil {
			return err
		}
		return reply(w, 200, fields{"mode": policy.Mode, "value": policy.Value, "removeCount": removed, "protectedCount": protected, "head": head})
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO history_policies VALUES(?,?,?) ON CONFLICT(unit) DO UPDATE SET mode=excluded.mode,value=excluded.value", unit, policy.Mode, policy.Value); err != nil {
		return err
	}
	if err = s.applyPolicy(ctx, tx, unit, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, 200, policy)
}

type timelineInput struct {
	Snapshot string `json:"snapshotId"`
	Head     string `json:"expectedHead"`
}

func (in timelineInput) valid() bool {
	return validID.MatchString(in.Snapshot) && validID.MatchString(in.Head)
}
func validateDelete(ctx context.Context, tx *sql.Tx, unit, target string) (bool, error) {
	var deleted bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM snapshot_tombstones WHERE unit=? AND snapshot=?)", unit, target).Scan(&deleted); err != nil || deleted {
		return deleted, err
	}
	var protected bool
	args := append(policyArgs(unit, historyPolicy{Mode: "all"}, time.Now()), sql.Named("target", target))
	err := tx.QueryRowContext(ctx, retentionRows+"SELECT protected FROM decision WHERE id=:target", args...).Scan(&protected)
	if err != nil {
		return false, err
	}
	if protected {
		return false, conflict("当前主线、未合并分叉或有效保留中的时间点无法删除")
	}
	return false, nil
}
func (s *Store) deletePoint(w http.ResponseWriter, r *http.Request, dev string) error {
	var in struct {
		timelineInput
		PlanToken string `json:"planToken,omitempty"`
	}
	if err := decode(r, &in, 4096); err != nil {
		return err
	}
	if !in.valid() {
		return bad("invalid snapshot or expected head")
	}
	ctx, unit := r.Context(), r.PathValue("unitId")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	head, err := timelineHead(ctx, tx, unit, dev, true)
	if err != nil {
		return err
	}
	// A lost reply can be replayed only by the device that committed this
	// exact preview. An ACK remains valid even if a new head has appeared.
	var receipt []byte
	err = tx.QueryRowContext(ctx, "SELECT receipt FROM delete_receipts WHERE unit=? AND snapshot=? AND device=? AND head=? AND token=?", unit, in.Snapshot, dev, in.Head, in.PlanToken).Scan(&receipt)
	if err == nil {
		return reply(w, 200, json.RawMessage(receipt))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if head != in.Head {
		return conflict("主线已变化，请刷新时间线")
	}
	deleted, err := validateDelete(ctx, tx, unit, in.Snapshot)
	if err != nil {
		return err
	}
	if deleted {
		return absent()
	}
	if !validDigest.MatchString(in.PlanToken) {
		return bad("请先预览删除及重建并提供有效计划编号")
	}
	if err = s.checkDeletePlan(ctx, tx, unit, in.Snapshot, head, in.PlanToken); err != nil {
		return err
	}
	points, err := s.rebuildHistory(ctx, tx, unit, []string{in.Snapshot})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO snapshot_tombstones VALUES(?,?,?,'manual')", unit, in.Snapshot, stamp(time.Now())); err != nil {
		return err
	}
	receipt, err = json.Marshal(fields{"snapshotId": in.Snapshot, "deleted": true, "rebuiltPoints": points})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO delete_receipts VALUES(?,?,?,?,?,?)", unit, in.Snapshot, dev, head, in.PlanToken, receipt); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, 200, json.RawMessage(receipt))
}
func (s *Store) restorePoint(w http.ResponseWriter, r *http.Request, dev string) error {
	var in struct {
		timelineInput
		Request string `json:"requestId"`
		Label   string `json:"label"`
	}
	if err := decode(r, &in, 8192); err != nil {
		return err
	}
	if !in.valid() || !validID.MatchString(in.Request) || utf8.RuneCountInString(in.Label) > 500 {
		return bad("invalid restore request")
	}
	ctx, unit := r.Context(), r.PathValue("unitId")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	head, err := timelineHead(ctx, tx, unit, dev, true)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(in)
	key := hash(body)
	var saved string
	var receipt []byte
	err = tx.QueryRowContext(ctx, "SELECT bodyhash,receipt FROM restore_requests WHERE unit=? AND device=? AND request=?", unit, dev, in.Request).Scan(&saved, &receipt)
	if err == nil {
		if key != saved {
			return conflict("请求编号已用于其他恢复操作")
		}
		return reply(w, 200, json.RawMessage(receipt))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if head != in.Head {
		return conflict("主线已变化，请重新确认恢复")
	}
	var root string
	var attachments, links []byte
	var gitHead, gitBranch *string
	var revision int
	err = tx.QueryRowContext(ctx, `SELECT root,COALESCE(json_extract(record,'$.attachments'),'[]'),COALESCE(json_extract(record,'$.links'),'[]'),COALESCE(json_extract(record,'$.scopeRevision'),1),json_extract(record,'$.gitHead'),json_extract(record,'$.gitBranch') FROM visible_snapshots WHERE unit=? AND id=?`, unit, in.Snapshot).Scan(&root, &attachments, &links, &revision, &gitHead, &gitBranch)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	at, id := stamp(now), newID()
	record := fields{"snapshotId": id, "unitId": unit, "deviceId": dev, "parentSnapshotId": head, "restoredFromSnapshotId": in.Snapshot, "rootDigest": root, "scopeRevision": revision, "label": in.Label, "checkpoint": true, "outcome": "published", "capturedAt": at, "committedAt": at, "attachments": json.RawMessage(attachments), "links": json.RawMessage(links)}
	if gitHead != nil {
		record["gitHead"] = *gitHead
	}
	if gitBranch != nil {
		record["gitBranch"] = *gitBranch
	}
	raw, _ := json.Marshal(record)
	if _, err = tx.ExecContext(ctx, "INSERT INTO snapshots(unit,id,root,record) VALUES(?,?,?,?)", unit, id, root, raw); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO refs SELECT unit,?,kind,digest FROM refs WHERE unit=? AND snapshot=?", id, unit, in.Snapshot); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE units SET head=? WHERE id=?", id, unit); err != nil {
		return err
	}
	receipt, _ = json.Marshal(fields{"unitId": unit, "snapshotId": id, "rootDigest": root, "outcome": "published", "headAtCommit": id, "committedAt": at})
	if _, err = tx.ExecContext(ctx, "INSERT INTO restore_requests VALUES(?,?,?,?,?)", unit, dev, in.Request, key, receipt); err != nil {
		return err
	}
	if err = s.projectNewHistory(ctx, tx, unit, id); err != nil {
		return err
	}
	if err = s.applyPolicy(ctx, tx, unit, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, 200, json.RawMessage(receipt))
}

func (s *Store) maintainRetention(ctx context.Context, now time.Time) error {
	if !s.volumeOK() {
		return errors.New("retention: data volume unavailable")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT unit FROM history_policies WHERE mode='days'")
	if err != nil {
		return err
	}
	var units []string
	for rows.Next() {
		var unit string
		if err = rows.Scan(&unit); err != nil {
			rows.Close()
			return err
		}
		units = append(units, unit)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, unit := range units {
		if err = ctx.Err(); err != nil {
			return err
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		if e = s.applyPolicy(ctx, tx, unit, now); e != nil {
			tx.Rollback()
		} else {
			e = tx.Commit()
		}
		if e != nil {
			failures = append(failures, fmt.Errorf("unit %s: %w", unit, e))
		}
	}
	return errors.Join(failures...)
}
func (s *Store) startRetention() {
	ctx, cancel := context.WithCancel(context.Background())
	s.retentionCancel = cancel
	s.retentionDone = make(chan struct{})
	go func() {
		defer close(s.retentionDone)
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			if err := s.maintainRetention(ctx, time.Now().UTC()); err != nil && ctx.Err() == nil {
				log.Printf("历史保留维护: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
