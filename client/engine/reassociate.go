package engine

import (
	"context"
	"database/sql"

	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func clearSourceSettings(ctx context.Context, tx *sql.Tx, unit string) error {
	for _, table := range []string{"ignored_selection", "binding_roots", "git_sources", "project_proofs", "checkout_base", "git_status_cache"} {
		if _, e := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE unit=?", unit); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) sharedTarget(ctx context.Context, remote *Remote, id string) (*string, error) {
	var units []unitInfo
	if err := remote.unitList(ctx, &units); err != nil {
		return nil, err
	}
	for _, u := range units {
		if u.ID == id && u.ScopeID != "" && u.Kind == "managed" && u.Role == "sync" {
			return u.Head, nil
		}
	}
	return nil, errors.New("需要所选共享项目的同步权限")
}
func baseID(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// ReassociateProject changes only the device's active source. A detached row
// remains the foreign-key parent of all of that unit's local receipts/snapshots.
func (s *Store) ReassociateProject(ctx context.Context, oldUnit, targetUnit, path, confirmedBase string) (Binding, error) {
	return s.reassociateWithPolicy(ctx, oldUnit, targetUnit, path, confirmedBase, nil)
}

func (s *Store) reassociateWithPolicy(ctx context.Context, oldUnit, targetUnit, path, confirmedBase string, requested *IgnoredPolicy) (Binding, error) {
	activity, lockErr := s.localLock(false)
	if lockErr != nil {
		return Binding{}, lockErr
	}
	defer activity.Close()
	var result Binding
	if validID(oldUnit) != nil || validID(targetUnit) != nil || oldUnit == targetUnit || path == "" {
		return result, errors.New("请明确选择原项目、不同的目标共享项目以及已绑定目录")
	}
	full, e := filepath.Abs(path)
	if e != nil {
		return result, e
	}
	full, e = filepath.EvalSymlinks(full)
	if e != nil {
		return result, e
	}
	if e = checkCredentialPath(full); e != nil {
		return result, e
	}
	remote, e := s.Remote(ctx)
	if e != nil {
		return result, e
	}
	head, e := s.sharedTarget(ctx, remote, targetUnit)
	if e != nil {
		return result, e
	}
	// Exact replay may follow a successful commit whose response was lost. Never
	// reset the target baseline or disturb edits captured since the original call.
	var applied bool
	if e = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM reassociations r JOIN bindings b ON b.path=r.path AND b.unit=r.target AND b.generation=r.generation WHERE r.path=? AND r.previous=? AND r.target=? AND r.confirmed=?)`, full, oldUnit, targetUnit, confirmedBase).Scan(&applied); e != nil {
		return result, e
	}
	if applied {
		return s.binding(ctx, targetUnit)
	}
	if baseID(head) != confirmedBase {
		return result, errors.New("目标主线已变化，请刷新并确认当前版本")
	}
	old, e := s.binding(ctx, oldUnit)
	if e != nil || old.Profile != "project" || old.Path != full {
		return result, errors.New("该目录与明确指定的原项目绑定不符")
	}
	var queued, occupied, previouslyGit bool
	var marker string
	if e = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM snapshots WHERE unit IN (?,?) AND state='queued'),
 EXISTS(SELECT 1 FROM bindings WHERE unit=? AND path IS NOT NULL), EXISTS(SELECT 1 FROM git_sources WHERE unit=?),
 identity FROM binding_roots WHERE unit=?`, oldUnit, targetUnit, targetUnit, oldUnit, oldUnit).Scan(&queued, &occupied, &previouslyGit, &marker); e != nil {
		return result, e
	}
	if queued {
		return result, errors.New("仍有待传快照，请先完成上传再更换项目")
	}
	if occupied {
		return result, errors.New("目标项目已绑定另一个本机目录")
	}
	verifyMarker := func() error {
		real, err := directoryMarkerAt(full)
		if err != nil || real != marker {
			return errors.New("本机工作区目录已被替换或卸载")
		}
		return nil
	}
	if e = verifyMarker(); e != nil {
		return result, e
	}
	ignored, e := readIgnored(ctx, s.db, oldUnit)
	if e != nil {
		return result, e
	}
	targetPolicy := ignored
	if requested != nil {
		targetPolicy = *requested
	}
	info, gitErr := os.Lstat(filepath.Join(full, ".git"))
	isGit := gitErr == nil && (info.IsDir() || info.Mode().IsRegular())
	if gitErr != nil && !os.IsNotExist(gitErr) {
		return result, gitErr
	}
	if (gitErr == nil && !isGit) || (previouslyGit && !isGit) {
		return result, errors.New("原项目的 Git 工作区标记缺失或类型发生变化")
	}
	result = Binding{Unit: targetUnit, Path: full, Profile: "project", Base: head, generation: newID()}
	var expected, observed string
	if head != nil {
		var record struct {
			Digest string `json:"contentDigest"`
		}
		if e = remote.call(ctx, "GET", "/units/"+targetUnit+"/snapshots/"+*head, fingerprintQuery, nil, &record); e != nil {
			return result, e
		}
		expected = record.Digest
		if len(expected) != 64 {
			return result, errors.New("目标服务不支持共享项目基线验证")
		}
		if observed, e = baselineFingerprint(ctx, result, targetPolicy); e != nil {
			return result, e
		}
		result.ProtectHead = expected != observed
	}
	// Capturing the local tree may take a while: check the target again. A remote
	// change after this read will still be caught by the upload's expectedHead.
	current, e := s.sharedTarget(ctx, remote, targetUnit)
	if e != nil {
		return result, e
	}
	if baseID(current) != confirmedBase {
		return result, errors.New("核对期间目标版本已变化，请刷新后重试")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return result, e
	}
	defer tx.Rollback()
	// This first write obtains the SQLite writer lock. It also rejects a racing
	// capture, another reassociation, and an A → B → A change during verification.
	archived, e := tx.ExecContext(ctx, `UPDATE bindings SET path=NULL,generation=? WHERE unit=? AND path=? AND generation=? AND base IS ? AND profile='project'
 AND NOT EXISTS(SELECT 1 FROM snapshots WHERE unit IN (?,?) AND state='queued')
 AND NOT EXISTS(SELECT 1 FROM bindings WHERE unit=? AND path IS NOT NULL)`, newID(), oldUnit, full, old.generation, old.Base, oldUnit, targetUnit, targetUnit)
	if e != nil {
		return result, e
	}
	n, e := archived.RowsAffected()
	if e != nil {
		return result, e
	}
	if n != 1 {
		return result, errors.New("确认期间绑定、目标或待传队列已变化，请刷新后重试")
	}
	if e = verifyMarker(); e != nil {
		return result, e
	}
	actual, e := readIgnored(ctx, tx, oldUnit)
	if e != nil {
		return result, e
	}
	if !ignored.equal(actual) {
		return result, errors.New("忽略文件选择已变化，请刷新后重新确认")
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO bindings(unit,path,profile,base,generation) VALUES(?,?,'project',?,?) ON CONFLICT(unit) DO UPDATE SET path=excluded.path,profile=excluded.profile,base=excluded.base,generation=excluded.generation`, targetUnit, full, head, result.generation); e != nil {
		return result, e
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM git_status_cache WHERE unit=?", oldUnit); e != nil {
		return result, e
	}
	// Old restore intents refer to the previous association's working directory.
	if _, e = tx.ExecContext(ctx, "DELETE FROM checkout_journal WHERE unit IN (?,?)", oldUnit, targetUnit); e != nil {
		return result, e
	}
	if e = clearSourceSettings(ctx, tx, targetUnit); e != nil {
		return result, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO binding_roots VALUES(?,?)", targetUnit, marker); e != nil {
		return result, e
	}
	if isGit {
		if _, e = tx.ExecContext(ctx, "INSERT INTO git_sources VALUES(?)", targetUnit); e != nil {
			return result, e
		}
	}
	if err := writeIgnored(ctx, tx, targetUnit, targetPolicy); err != nil {
		return result, err
	}
	if head != nil {
		if _, e = tx.ExecContext(ctx, "INSERT INTO project_proofs VALUES(?,?,?,?)", targetUnit, *head, expected, observed); e != nil {
			return result, e
		}
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO reassociations VALUES(?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET previous=excluded.previous,target=excluded.target,confirmed=excluded.confirmed,generation=excluded.generation`, full, oldUnit, targetUnit, confirmedBase, result.generation); e != nil {
		return result, e
	}
	if e = tx.Commit(); e != nil {
		return result, fmt.Errorf("更换项目关联: %w", e)
	}
	return result, nil
}
