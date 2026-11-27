package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

// Each tombstone with this cause redirects only to its immutable, verified
// first parent. Ordinary removals and restores never confer that permission.
const redundantHeadCause = "redundant-head"

type collapseReport struct {
	Unit    string         `json:"unitId"`
	OldHead string         `json:"expectedHead"`
	Keep    string         `json:"retainedHead"`
	Remove  []string       `json:"removedSnapshotIds"`
	Rebuilt []rebuiltPoint `json:"rebuiltPoints"`
	Applied bool           `json:"applied"`
}

type collapsePoint struct {
	Parent     *string `json:"parentSnapshotId"`
	Root       string  `json:"rootDigest"`
	Label      string  `json:"label"`
	Checkpoint *bool   `json:"checkpoint"`
	Outcome    string  `json:"outcome"`
	Merge      string  `json:"mergeParentSnapshotId"`
	Restored   string  `json:"restoredFromSnapshotId"`
	metadata   map[string]any
}

func readCollapsePoint(ctx context.Context, tx *sql.Tx, unit, id string) (collapsePoint, error) {
	var p collapsePoint
	var raw []byte
	var root string
	if err := tx.QueryRowContext(ctx, "SELECT root,record FROM visible_snapshots WHERE unit=? AND id=?", unit, id).Scan(&root, &raw); err != nil {
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if root != p.Root || !validDigest.MatchString(root) {
		return p, errors.New("invalid snapshot root")
	}
	if err := json.Unmarshal(raw, &p.metadata); err != nil {
		return p, err
	}
	// Compare every other publication field, including attachments, Git
	// state, links and future project metadata. Preserve named/explicit tips.
	for _, key := range []string{"snapshotId", "unitId", "rootDigest", "parentSnapshotId", "deviceId", "capturedAt", "committedAt", "outcome", "label", "checkpoint"} {
		delete(p.metadata, key)
	}
	return p, nil
}

// Requires the complete ordered list from the observed head to the caller's
// explicit surviving parent. All proofs and writes share an immediate SQLite
// transaction, excluding concurrent publication, hold or retention changes.
func (s *Store) collapseHeads(ctx context.Context, unit, head, keep string, ids []string, apply bool) (collapseReport, error) {
	report := collapseReport{Unit: unit, OldHead: head, Keep: keep, Remove: ids}
	if !s.volumeOK() || !validID.MatchString(unit) || !validID.MatchString(head) || !validID.MatchString(keep) || len(ids) == 0 || ids[0] != head {
		return report, errors.New("require a mounted store, --unit, --head, --keep and ordered --remove IDs starting with the head")
	}
	seen := map[string]bool{keep: true}
	for _, id := range ids {
		if !validID.MatchString(id) || seen[id] {
			return report, errors.New("invalid or repeated removal ID")
		}
		seen[id] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	var current string
	var project bool
	err = tx.QueryRowContext(ctx, "SELECT head,EXISTS(SELECT 1 FROM projects WHERE unit=u.id) FROM units u WHERE id=? AND NOT EXISTS(SELECT 1 FROM external_roots WHERE unit=u.id)", unit).Scan(&current, &project)
	if err != nil {
		return report, err
	}
	if current != head {
		return report, conflict("主线已变化，请重新核对清理范围")
	}
	load := func(ref objectfmt.Ref) ([]byte, error) { return os.ReadFile(s.path(ref)) }
	next, err := readCollapsePoint(ctx, tx, unit, keep)
	if err != nil {
		return report, err
	}
	if next.Outcome != "published" {
		return report, errors.New("surviving head must be published")
	}
	parentID := keep
	for i := len(ids) - 1; i >= 0; i-- {
		id := ids[i]
		point, err := readCollapsePoint(ctx, tx, unit, id)
		if err != nil {
			return report, err
		}
		if point.Parent == nil || *point.Parent != parentID || point.Outcome != "published" || point.Checkpoint == nil || *point.Checkpoint || point.Label != "" || point.Merge != "" || point.Restored != "" {
			return report, fmt.Errorf("时间点 %s 不属于指定的连续自动主线", id)
		}
		var protected bool
		err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM holds WHERE unit=?1 AND snapshot=?2 AND released=0) OR EXISTS(SELECT 1 FROM timeline_bases WHERE unit=?1 AND snapshot=?2)", unit, id).Scan(&protected)
		if err != nil {
			return report, err
		}
		if protected {
			return report, fmt.Errorf("时间点 %s 存在有效保留或重建记录", id)
		}
		if !reflect.DeepEqual(point.metadata, next.metadata) {
			return report, fmt.Errorf("时间点 %s 的项目元数据有变化", id)
		}
		equal, err := objectfmt.SameTreeContents(ctx, next.Root, point.Root, project, load, load)
		if err != nil {
			return report, err
		}
		if !equal {
			return report, fmt.Errorf("时间点 %s 包含有意义的文件变化", id)
		}
		next, parentID = point, id
	}
	if !apply {
		report.Rebuilt, err = plannedRebuild(ctx, tx, unit, ids)
		return report, err
	}
	if report.Rebuilt, err = s.rebuildHistory(ctx, tx, unit, ids); err != nil {
		return report, err
	}
	affected, err := tx.ExecContext(ctx, "UPDATE units SET head=? WHERE id=? AND head=?", keep, unit, head)
	if err != nil {
		return report, err
	}
	if n, err := affected.RowsAffected(); err != nil || n != 1 {
		return report, errors.New("主线已变化")
	}
	at := stamp(time.Now())
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "INSERT INTO snapshot_tombstones VALUES(?,?,?,?)", unit, id, at, redundantHeadCause); err != nil {
			return report, err
		}
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	report.Applied = true
	return report, nil
}

func collapsedBaseMatches(ctx context.Context, tx *sql.Tx, unit, oldBase, head string) (bool, error) {
	// Only follow admin-verified redirects. A genuine intervening publish
	// still takes the normal fork path. The shared-project proof is checked
	// against the original base the uploading device actually worked from.
	var matches bool
	err := tx.QueryRowContext(ctx, `WITH RECURSIVE chain(id) AS (
	SELECT ?2 UNION SELECT json_extract(s.record,'$.parentSnapshotId') FROM chain c
	JOIN snapshot_tombstones t ON t.unit=?1 AND t.snapshot=c.id AND t.cause=?4
	JOIN snapshots s ON s.unit=t.unit AND s.id=t.snapshot)
	SELECT EXISTS(SELECT 1 FROM chain WHERE id=?3)`, unit, oldBase, head, redundantHeadCause).Scan(&matches)
	return matches, err
}
