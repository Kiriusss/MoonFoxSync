package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

// Publication facts and receipts are immutable. The user-visible ancestry can
// contract across removed checkpoints without changing a surviving snapshot ID.
const schemaHistoryRebuild = `
CREATE TABLE IF NOT EXISTS timeline_bases(unit TEXT NOT NULL,snapshot TEXT NOT NULL,parents TEXT NOT NULL,
 PRIMARY KEY(unit,snapshot),FOREIGN KEY(unit,snapshot) REFERENCES snapshots(unit,id));
CREATE VIEW IF NOT EXISTS timeline_history AS
 SELECT s.seq,s.unit,s.id,s.root,
 CASE WHEN b.snapshot IS NULL THEN s.record
 WHEN json_array_length(b.parents)>1 THEN json_set(s.record,'$.parentSnapshotId',json_extract(b.parents,'$[0]'),
 '$.mergeParentSnapshotId',json_extract(b.parents,'$[1]'),'$.timelineParentSnapshotIds',json(b.parents),'$.timelineRebuilt',json('true'))
 ELSE json_remove(json_set(s.record,'$.parentSnapshotId',json_extract(b.parents,'$[0]'),
 '$.timelineParentSnapshotIds',json(b.parents),'$.timelineRebuilt',json('true')),'$.mergeParentSnapshotId') END AS record
 FROM visible_snapshots s LEFT JOIN timeline_bases b ON b.unit=s.unit AND b.snapshot=s.id;
`

type rebuiltPoint struct {
	Snapshot string   `json:"snapshotId"`
	Label    string   `json:"label"`
	Before   []string `json:"parentsBefore"`
	After    []string `json:"parentsAfter"`
}
type deletionPlan struct {
	Snapshot  string         `json:"snapshotId"`
	Head      string         `json:"expectedHead"`
	Token     string         `json:"planToken"`
	Protected string         `json:"protectedReason,omitempty"`
	Points    []rebuiltPoint `json:"rebuiltPoints"`
}

// Process the DAG in publication order. Replacing each removed parent by its
// nearest surviving parents handles adjacent removals and merges with >2
// remaining parents. Parent order keeps the first-parent view deterministic.
func plannedRebuild(ctx context.Context, tx *sql.Tx, unit string, removing []string) ([]rebuiltPoint, error) {
	remove := make(map[string]bool, len(removing))
	for _, id := range removing {
		remove[id] = true
	}
	rows, e := tx.QueryContext(ctx, `SELECT s.id,COALESCE(json_extract(s.record,'$.label'),''),
 json_extract(s.record,'$.parentSnapshotId'),json_extract(s.record,'$.mergeParentSnapshotId'),
 b.parents,t.snapshot IS NOT NULL FROM snapshots s
 LEFT JOIN timeline_bases b ON b.unit=s.unit AND b.snapshot=s.id
 LEFT JOIN snapshot_tombstones t ON t.unit=s.unit AND t.snapshot=s.id
 WHERE s.unit=? ORDER BY s.seq`, unit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []rebuiltPoint{}
	hiddenParents := map[string][]string{}
	for rows.Next() {
		var id, label string
		var first, merge, overlay *string
		var hidden bool
		if e = rows.Scan(&id, &label, &first, &merge, &overlay, &hidden); e != nil {
			return nil, e
		}
		before := []string{}
		if overlay != nil {
			if e = json.Unmarshal([]byte(*overlay), &before); e != nil {
				return nil, e
			}
		} else {
			if first != nil {
				before = append(before, *first)
			}
			if merge != nil && (first == nil || *merge != *first) {
				before = append(before, *merge)
			}
		}
		after := make([]string, 0, len(before))
		seen := map[string]bool{}
		add := func(parent string) {
			if !seen[parent] {
				after = append(after, parent)
				seen[parent] = true
			}
		}
		for _, p := range before {
			if ancestors, ok := hiddenParents[p]; ok {
				for _, ancestor := range ancestors {
					add(ancestor)
				}
			} else {
				add(p)
			}
		}
		if hidden || remove[id] {
			hiddenParents[id] = after
			continue
		}
		if !slices.Equal(after, before) {
			out = append(out, rebuiltPoint{Snapshot: id, Label: label, Before: before, After: after})
		}
	}
	return out, rows.Err()
}

func planToken(unit, snapshot, head string, points []rebuiltPoint) string {
	raw, _ := json.Marshal([]any{unit, snapshot, head, points})
	return hash(raw)
}
func (s *Store) checkDeletePlan(ctx context.Context, tx *sql.Tx, unit, target, head, token string) error {
	points, e := plannedRebuild(ctx, tx, unit, []string{target})
	if e != nil {
		return e
	}
	if planToken(unit, target, head, points) != token {
		return conflict("受影响的时间点已变化，请重新预览删除与重建")
	}
	return nil
}
func (s *Store) previewDelete(w http.ResponseWriter, r *http.Request, dev string) error {
	unit := r.PathValue("unitId")
	var in struct {
		Snapshot string `json:"snapshotId"`
		Head     string `json:"expectedHead"`
	}
	if err := decode(r, &in, 4096); err != nil {
		return err
	}
	if !validID.MatchString(in.Snapshot) || !validID.MatchString(in.Head) {
		return bad("invalid snapshot or head")
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	head, err := timelineHead(r.Context(), tx, unit, dev, true)
	if err != nil {
		return err
	}
	if head != in.Head {
		return conflict("主线已变化，请刷新版本历史")
	}
	plan := deletionPlan{Snapshot: in.Snapshot, Head: head, Points: []rebuiltPoint{}}
	deleted, err := validateDelete(r.Context(), tx, unit, in.Snapshot)
	if err != nil {
		var p *problem
		if errors.As(err, &p) && p.Status == 409 {
			plan.Protected = p.Message
			return reply(w, 200, plan)
		}
		return err
	}
	if deleted {
		return absent()
	}
	plan.Points, err = plannedRebuild(r.Context(), tx, unit, []string{in.Snapshot})
	if err != nil {
		return err
	}
	plan.Token = planToken(unit, in.Snapshot, head, plan.Points)
	return reply(w, 200, plan)
}

func saveVisibleParents(ctx context.Context, tx *sql.Tx, unit string, point rebuiltPoint) error {
	raw, e := json.Marshal(point.After)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO timeline_bases(unit,snapshot,parents) VALUES(?,?,?)
 ON CONFLICT(unit,snapshot) DO UPDATE SET parents=excluded.parents`, unit, point.Snapshot, string(raw))
	return e
}

// An offline device can legitimately publish a fork based on a point removed
// yesterday. Its new publication already has a full object index; map its
// ancestry before exposing that new fork to timeline readers.
func (s *Store) projectNewHistory(ctx context.Context, tx *sql.Tx, unit, id string) error {
	var stale bool
	e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM snapshots s JOIN snapshot_tombstones t ON t.unit=s.unit
 AND t.snapshot IN (json_extract(s.record,'$.parentSnapshotId'),json_extract(s.record,'$.mergeParentSnapshotId'))
 WHERE s.unit=? AND s.id=?)`, unit, id).Scan(&stale)
	if e != nil || !stale {
		return e
	}
	points, e := plannedRebuild(ctx, tx, unit, nil)
	if e != nil {
		return e
	}
	for _, p := range points {
		if p.Snapshot == id {
			return saveVisibleParents(ctx, tx, unit, p)
		}
	}
	return nil
}

func (s *Store) rebuildHistory(ctx context.Context, tx *sql.Tx, unit string, removing []string) ([]rebuiltPoint, error) {
	if len(removing) == 0 {
		return []rebuiltPoint{}, nil
	}
	points, e := plannedRebuild(ctx, tx, unit, removing)
	if e != nil {
		return nil, e
	}
	for _, p := range points {
		if e = s.rebuildPoint(ctx, tx, unit, p); e != nil {
			return nil, fmt.Errorf("重建时间点 %s 失败：%w", p.Snapshot, e)
		}
	}
	return points, nil
}
func (s *Store) rebuildPoint(ctx context.Context, tx *sql.Tx, unit string, p rebuiltPoint) error {
	var root string
	var attachments []byte
	if e := tx.QueryRowContext(ctx, `SELECT root,COALESCE(json_extract(record,'$.attachments'),'[]') FROM snapshots WHERE unit=? AND id=?`, unit, p.Snapshot).Scan(&root, &attachments); e != nil {
		return e
	}
	var assets []asset
	if e := json.Unmarshal(attachments, &assets); e != nil {
		return e
	}
	refs := map[objectfmt.Ref]bool{}
	load := func(o objectfmt.Ref) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := s.read(o)
		if err == nil {
			refs[o] = true
		}
		return b, err
	}
	if e := objectfmt.VerifyTree(root, load); e != nil {
		return e
	}
	for _, a := range assets {
		if e := objectfmt.VerifyFile(a.FileDigest, load); e != nil {
			return e
		}
	}
	// Replace the whole reachability index with the verified closure. Its object
	// access and restore operations are now independent even of the removed refs.
	if _, e := tx.ExecContext(ctx, "DELETE FROM refs WHERE unit=? AND snapshot=?", unit, p.Snapshot); e != nil {
		return e
	}
	insert, e := tx.PrepareContext(ctx, "INSERT INTO refs(unit,snapshot,kind,digest) VALUES(?,?,?,?)")
	if e != nil {
		return e
	}
	defer insert.Close()
	for ref := range refs {
		if _, e = insert.ExecContext(ctx, unit, p.Snapshot, ref.Kind, ref.Digest); e != nil {
			return e
		}
	}
	return saveVisibleParents(ctx, tx, unit, p)
}
