package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type ProjectInfo struct {
	SourceKey string  `json:"sourceKey,omitempty"`
	UnitID    string  `json:"unitId"`
	ScopeID   string  `json:"scopeId"`
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Head      *string `json:"head"`
	Created   bool    `json:"created"`
}

func (r *Remote) EnsureProject(ctx context.Context, scope, key, name string) (ProjectInfo, error) {
	return r.registerProject(ctx, scope, key, name, "")
}

func (r *Remote) NewProject(ctx context.Context, scope, requestID, name, origin string) (ProjectInfo, error) {
	return r.registerProject(ctx, scope, "new:"+requestID, name, origin)
}

func (r *Remote) registerProject(ctx context.Context, scope, key, name, origin string) (ProjectInfo, error) {
	var p ProjectInfo
	if err := validID(scope); err != nil {
		return p, err
	}
	doc := map[string]string{"key": key, "name": name}
	if origin != "" {
		doc["sourceKey"] = origin
	}
	err := r.call(ctx, "POST", "/units/"+scope+"/projects", nil, doc, &p)
	if err == nil && (p.ScopeID != scope || p.Key != key || validID(p.UnitID) != nil) {
		err = errors.New("服务端返回的项目身份不一致")
	}
	return p, err
}

func (r *Remote) ExistingProject(ctx context.Context, scope, target string) (ProjectInfo, error) {
	var p ProjectInfo
	if err := validID(target); err != nil {
		return p, err
	}
	items, err := r.Projects(ctx, scope)
	if err != nil {
		return p, err
	}
	for _, raw := range items.Items {
		if err = json.Unmarshal(raw, &p); err != nil {
			return ProjectInfo{}, err
		}
		if p.UnitID == target && p.ScopeID == scope {
			return p, nil
		}
	}
	return ProjectInfo{}, errors.New("所选项目不在当前已授权共享空间；请刷新项目清单")
}
func (r *Remote) Projects(ctx context.Context, scope string) (Page, error) {
	if err := validID(scope); err != nil {
		return Page{}, err
	}
	return r.pages(ctx, "/units/"+scope+"/projects", nil)
}

type checkoutRecord struct {
	ContentDigest string `json:"contentDigest,omitempty"`
	Receipt
	Parent   *string `json:"parentSnapshotId"`
	Label    string  `json:"label"`
	Captured string  `json:"capturedAt"`
}
type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Check every binding, including the one being switched: a new checkout must never live
// within the old working tree or become an ancestor of another source.
func checkoutPrior(ctx context.Context, q queryer, unit, dst string) (Binding, error) {
	var prior Binding
	rows, e := q.QueryContext(ctx, "SELECT unit,path,profile,base,generation FROM bindings WHERE path IS NOT NULL")
	if e != nil {
		return prior, e
	}
	defer rows.Close()
	for rows.Next() {
		var b Binding
		if e = rows.Scan(&b.Unit, &b.Path, &b.Profile, &b.Base, &b.generation); e != nil {
			return prior, e
		}
		if b.Unit == unit {
			prior = b
			if b.Profile != "project" {
				return prior, errors.New("该项目绑定为单个文件，无法切换项目工作树")
			}
		}
		if b.Unit == unit && b.Path == dst {
			continue
		}
		if inside(b.Path, dst) || inside(dst, b.Path) {
			return prior, errors.New("目标目录与已绑定来源重叠")
		}
	}
	if e = rows.Err(); e != nil {
		return prior, e
	}
	rows.Close()
	var queued bool
	if e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM snapshots WHERE unit=? AND state='queued')", unit).Scan(&queued); e != nil {
		return prior, e
	}
	if queued {
		return prior, errors.New("项目仍有待上传快照，请发布后再切换工作树；也可将远端版本单独恢复到新目录")
	}
	return prior, nil
}
func bindingStamp(b Binding) string { raw, _ := json.Marshal(b); return string(raw) + b.generation }

// CheckoutProject restores the authoritative head to a new directory then explicitly
// moves the local binding. Never delete or modify the previous source, including .git,
// ignored files and edits not yet captured. Immutable versions make concurrent remote
// publishes safe: the next push from a stale base becomes a recoverable fork.
func (s *Store) CheckoutProject(ctx context.Context, unit, destination string) (Binding, error) {
	activity, lockErr := s.projectActivity(unit)
	if lockErr != nil {
		return Binding{}, lockErr
	}
	defer activity.Close()
	if err := s.guardWorkspace(ctx, unit); err != nil {
		return Binding{}, err
	}
	var result Binding
	if e := validID(unit); e != nil {
		return result, e
	}
	if destination == "" {
		return result, errors.New("需要选择新目录")
	}
	abs, e := filepath.Abs(destination)
	if e != nil {
		return result, e
	}
	parent, e := filepath.EvalSymlinks(filepath.Dir(abs))
	if e != nil {
		return result, e
	}
	dst := filepath.Join(parent, filepath.Base(abs))
	// A crashed process releases this OS lock. A missing destination can then
	// safely start again, even if the previous process committed only its intent.
	lockName := fmt.Sprintf("checkout-%x.lock", sha256.Sum256([]byte(dst)))
	lock, e := cacheLock(filepath.Join(s.Home, lockName))
	if e != nil {
		return result, fmt.Errorf("目标目录正在检出或无法锁定: %w", e)
	}
	defer lock.Close()
	if inside(s.Home, dst) || inside(dst, s.Home) {
		return result, errors.New("检出目录不能与客户端状态目录重叠")
	}
	if e = checkCredentialPath(dst); e != nil {
		return result, e
	}
	remote, e := s.Remote(ctx)
	if e != nil {
		return result, e
	}
	var units []unitInfo
	if e = remote.unitList(ctx, &units); e != nil {
		return result, e
	}
	var head string
	for _, u := range units {
		if u.ID == unit && u.Kind == "managed" && u.Role == "sync" && u.Head != nil {
			head = *u.Head
			break
		}
	}
	if head == "" {
		return result, errors.New("项目需要已发布版本和同步权限")
	}
	prior, e := checkoutPrior(ctx, s.db, unit, dst)
	if e != nil {
		return result, e
	}

	var marker, localRoot, original string
	var record []byte
	var info checkoutRecord
	if _, e = os.Lstat(dst); e == nil {
		if e = s.db.QueryRowContext(ctx, "SELECT prior,marker,local_root,record FROM checkout_journal WHERE unit=? AND path=?", unit, dst).Scan(&original, &marker, &localRoot, &record); e != nil {
			return result, errors.New("目标已存在，请选择一个尚不存在的新目录")
		}
		if found, err := directoryMarkerAt(dst); err != nil || found != marker {
			return result, errors.New("检出目录已被替换，请选择新的目标")
		}
		if prior.Path == dst {
			return prior, nil
		}
		if bindingStamp(prior) != original {
			return result, errors.New("绑定已变化，请选择新的目标")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return result, e
	} else {
		if e = remote.call(ctx, "GET", "/units/"+unit+"/snapshots/"+head, fingerprintQuery, nil, &info); e != nil {
			return result, e
		}
		if info.Unit != unit || info.Snapshot != head || info.Outcome != "published" || len(info.Root) != 64 {
			return result, errors.New("远端主线版本不一致")
		}
		info.Head = &info.Snapshot
		record, e = json.Marshal(info)
		if e != nil {
			return result, e
		}
		stage, e := os.MkdirTemp(parent, ".xinhu-checkout-")
		if e != nil {
			return result, e
		}
		defer os.RemoveAll(stage)
		tree := filepath.Join(stage, "tree")
		if _, e = remote.Restore(ctx, unit, head, "", tree); e != nil {
			return result, e
		}
		if marker, e = directoryMarkerAt(tree); e != nil {
			return result, e
		}
		root, e := os.OpenRoot(tree)
		if e != nil {
			return result, e
		}
		c := capture{ctx: ctx, root: root}
		entry, e := c.entry(".")
		root.Close()
		if e != nil {
			return result, e
		}
		localRoot = entry.Digest
		original = bindingStamp(prior)
		// Commit the identity of the fully verified private directory before
		// publishing it. An interrupted rename can be resumed without accepting
		// an arbitrary pre-existing user directory.
		if _, e = s.db.ExecContext(ctx, `INSERT INTO checkout_journal VALUES(?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET unit=excluded.unit,prior=excluded.prior,marker=excluded.marker,local_root=excluded.local_root,record=excluded.record`, unit, dst, original, marker, localRoot, record); e != nil {
			return result, e
		}
		if e = publishNew(tree, dst); e != nil {
			_, _ = s.db.ExecContext(ctx, "DELETE FROM checkout_journal WHERE path=? AND marker=?", dst, marker)
			return result, e
		}
		if e = syncDirectory(parent); e != nil {
			return result, e
		}
	}
	if e = json.Unmarshal(record, &info); e != nil {
		return result, e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return result, e
	}
	defer tx.Rollback()
	// Acquire the writer lock before reading local bindings so a racing scheduler,
	// bind or checkout cannot advance an older worktree while we switch.
	if _, e = tx.ExecContext(ctx, "UPDATE checkout_journal SET marker=marker WHERE path=? AND marker=?", dst, marker); e != nil {
		return result, e
	}
	prior, e = checkoutPrior(ctx, tx, unit, dst)
	if e != nil {
		return result, e
	}
	if prior.Path == dst {
		return prior, nil
	}
	if bindingStamp(prior) != original {
		return result, fmt.Errorf("下载期间绑定或本地快照发生变化，已保留下载副本 %s", dst)
	}
	current, e := directoryMarkerAt(dst)
	if e != nil || current != marker {
		return result, errors.New("检出目标已被替换")
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO bindings(unit,path,profile,base,generation) VALUES(?,?,'project',?,?) ON CONFLICT(unit) DO UPDATE SET path=excluded.path,base=excluded.base,generation=excluded.generation,profile='project'`, unit, dst, info.Snapshot, newID()); e != nil {
		return result, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO binding_roots VALUES(?,?) ON CONFLICT(unit) DO UPDATE SET identity=excluded.identity`, unit, marker); e != nil {
		return result, e
	}
	// Git include overrides belong to the old source. A restored plain working
	// tree already contains the published files and has no Git ignore index.
	if _, e = tx.ExecContext(ctx, "DELETE FROM ignored_selection WHERE unit=?", unit); e != nil {
		return result, e
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM git_sources WHERE unit=?", unit); e != nil {
		return result, e
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM project_proofs WHERE unit=?", unit); e != nil {
		return result, e
	}
	if len(info.ContentDigest) == 64 {
		if _, e = tx.ExecContext(ctx, "INSERT INTO project_proofs VALUES(?,?,?,?)", unit, info.Snapshot, info.ContentDigest, info.ContentDigest); e != nil {
			return result, e
		}
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM git_status_cache WHERE unit=?", unit); e != nil {
		return result, e
	}
	receipt, e := json.Marshal(info.Receipt)
	if e != nil {
		return result, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO snapshots VALUES(?,?,?,?,?,?,?,'published',?) ON CONFLICT(id) DO NOTHING`, info.Snapshot, unit, info.Parent, info.Root, info.Label, info.Captured, newID(), receipt); e != nil {
		return result, e
	}
	var matches bool
	if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM snapshots WHERE id=? AND unit=? AND root=? AND state='published')", info.Snapshot, unit, info.Root).Scan(&matches); e != nil {
		return result, e
	}
	if !matches {
		return result, errors.New("本地快照身份与远端不符")
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO checkout_base VALUES(?,?,?) ON CONFLICT(unit) DO UPDATE SET snapshot=excluded.snapshot,local_root=excluded.local_root`, unit, info.Snapshot, localRoot); e != nil {
		return result, e
	}
	result = Binding{Unit: unit, Path: dst, Profile: "project", Base: &info.Snapshot}
	return result, tx.Commit()
}
