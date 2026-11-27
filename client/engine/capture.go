package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"path/filepath"

	"runtime"
	"strings"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

type Snapshot struct {
	ID       string  `json:"snapshotId"`
	Unit     string  `json:"unitId"`
	Parent   *string `json:"parentSnapshotId"`
	Root     string  `json:"rootDigest"`
	Label    string  `json:"label"`
	Captured string  `json:"capturedAt"`
	State    string  `json:"state"`
}
type Offer struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Size   int    `json:"size"`
}
type statCheck struct {
	path string
	info fs.FileInfo
}
type capture struct {
	files            objectfmt.FileBuilder
	index            *captureFileIndex
	installed        []objectfmt.Ref // Newly installed spool objects on a scheduled comparison.
	archives         map[string]WorkspaceFile
	deferredArchives map[string]workspaceDigest
	timeless         bool
	ctx              context.Context
	store            *Store
	root             *os.Root
	refs             map[objectfmt.Ref]int
	directoryChecks  []directoryCheck
	checks           []statCheck
	included         map[string]bool
	excluded         map[string]bool
}

func (s *Store) objectPath(r objectfmt.Ref) string {
	return filepath.Join(s.Home, "spool", r.Kind, r.Digest[:2], r.Digest)
}
func (c *capture) put(r objectfmt.Ref, data []byte) error {
	if c.store == nil || c.timeless {
		return c.ctx.Err()
	} // A restored worktree can be fingerprinted without copying the objects again.
	if e := c.ctx.Err(); e != nil {
		return e
	}
	if _, ok := c.refs[r]; ok {
		return nil
	}
	p := c.store.objectPath(r)
	dir := filepath.Dir(p)
	if st, e := os.Lstat(p); e == nil {
		if !st.Mode().IsRegular() || st.Size() != int64(len(data)) {
			return fmt.Errorf("损坏的缓存对象 %s", r.Digest)
		}
	} else if !os.IsNotExist(e) {
		return e
	} else {
		if e = os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		f, e := os.CreateTemp(dir, "incoming-")
		if e != nil {
			return e
		}
		defer os.Remove(f.Name())
		_, e = f.Write(data)
		if e == nil {
			e = f.Sync()
		}
		ec := f.Close()
		if e == nil {
			e = ec
		}
		if e != nil {
			return e
		}
		// The spool is content-addressed. Only install complete, durable objects.
		if e = os.Link(f.Name(), p); e != nil && !errors.Is(e, fs.ErrExist) {
			return e
		}
		if e == nil && c.installed != nil {
			c.installed = append(c.installed, r)
		}
		if e = syncDirectory(dir); e != nil {
			return e
		}
	}
	c.refs[r] = len(data)
	return nil
}

// readSpoolBytes is the loader for objectfmt decoders, which verify each page.
// Callers consuming bytes directly (such as uploads) must use load instead.
func (s *Store) readSpoolBytes(r objectfmt.Ref) ([]byte, error) {
	return os.ReadFile(s.objectPath(r))
}

func (s *Store) load(r objectfmt.Ref) ([]byte, error) {
	b, e := s.readSpoolBytes(r)
	if e != nil {
		return nil, e
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != r.Digest {
		return nil, fmt.Errorf("本机缓存校验失败: %s", r.Digest)
	}
	return b, nil
}

// Set both selection filters from one policy, for ordinary captures and shared
// project compatibility checks. A nil inclusion map walks the whole worktree,
// including empty directories and Git-ignored files created after import.
func (c *capture) projectMembers(dir string, policy IgnoredPolicy) (bool, error) {
	if st, err := c.root.Lstat(".git"); os.IsNotExist(err) {
		if len(policy.Items) > 0 {
			return false, errors.New("已登记的 Git 工作区标记缺失")
		}
		return false, nil
	} else if err != nil {
		return false, err
	} else if !st.IsDir() && !st.Mode().IsRegular() {
		return false, errors.New("不跟随 .git 符号链接")
	}
	if policy.Mode == "all" {
		if ok, err := gitValue(c.ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil || ok != "true" {
			return false, errors.New("无法验证 Git 工作区")
		}
		matches, err := ignoredMatches(c.ctx, dir, policy.Items)
		if err != nil {
			return false, err
		}
		c.excluded = make(map[string]bool, len(matches))
		for member := range matches {
			c.excluded[filepath.FromSlash(member)] = true
		}
		return true, nil
	}
	cmd := gitCommand(c.ctx, dir, "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", ".")
	raw, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("Git 工作区枚举失败: %w", err)
	}
	c.included = map[string]bool{}
	include := func(member string) {
		for p := filepath.FromSlash(member); p != "."; p = filepath.Dir(p) {
			c.included[p] = true
		}
	}
	for _, member := range strings.Split(string(raw), "\x00") {
		member = strings.TrimSuffix(member, "/") // git includes a slash for a nested repository
		if member == "" {
			continue
		}
		if !validMember(member) {
			return false, errors.New("Git 返回了无效文件位置")
		}
		include(member)
	}
	for _, member := range policy.Items {
		st, err := ignoredRegular(c.root, member)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0 {
			return false, fmt.Errorf("已选择的文件类型发生变化: %s", member)
		}
		include(member)
	}
	return true, nil
}
func sameFile(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && fileChangeTime(a) == fileChangeTime(b)
}
func (c *capture) entry(p string) (objectfmt.Entry, error) {
	info, err := c.root.Lstat(p)
	if err != nil {
		return objectfmt.Entry{}, err
	}
	if info.IsDir() {
		return c.entryAt(c.root, p, p, info, nil)
	}
	parent := filepath.Dir(p)
	cache, err := c.index.directory(c.ctx, c.store, parent)
	if err != nil {
		return objectfmt.Entry{}, err
	}
	c.checks = append(c.checks, statCheck{p, info})
	entry, err := c.entryAt(c.root, p, p, info, cache)
	if err == nil {
		err = c.index.finishDirectory(c.ctx, c.store, parent, cache)
	}
	return entry, err
}

func (c *capture) entryAt(parent *os.Root, name, p string, info fs.FileInfo, cache map[string]indexedFile) (objectfmt.Entry, error) {
	var result objectfmt.Entry
	if e := c.ctx.Err(); e != nil {
		return result, e
	}
	var e error
	archive, managed := c.archives[filepath.ToSlash(p)]
	if !managed && runtime.GOOS == "windows" { // prepareArchives already recorded every registered link, including excluded files.
		c.checks = append(c.checks, statCheck{p, info})
	}
	result.Name = filepath.Base(p)
	if !c.timeless {
		result.MtimeNS = info.ModTime().UnixNano()
	}
	if managed && info.Mode()&os.ModeSymlink == 0 {
		return result, fmt.Errorf("采集期间原位置按需文件已被替换：%s", p)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		result.Kind = "symlink"
		result.Target, e = parent.Readlink(name)
		if e != nil {
			return result, e
		}
		if managed {
			if result.Target != c.store.workspaceTarget(archive) {
				return result, fmt.Errorf("采集期间原位置按需链接已改变：%s", p)
			}
			delete(c.archives, filepath.ToSlash(p))
			return c.archivedEntry(archive)
		}
		if c.store != nil {
			target := result.Target
			if !filepath.IsAbs(target) {
				target = filepath.Join(c.root.Name(), filepath.Dir(p), target)
			}
			if inside(filepath.Join(c.store.Home, "workspace-volume"), target) {
				return result, fmt.Errorf("未登记的按需卷链接不能作为项目内容发布：%s", p)
			}
		}
	case info.Mode().IsRegular():
		result.Kind = "file"
		result.Executable = info.Mode().Perm()&0111 != 0
		if cached, ok := c.index.lookup(p, info, cache); ok {
			result.Digest = cached
			break
		}
		result.Digest, e = c.hashFile(parent, name, p, info)
		if e == nil {
			e = c.index.record(c.ctx, c.store, p, info, result.Digest, cache)
		}
	case info.IsDir():
		result.Kind = "directory"
		dir, err := parent.OpenRoot(name)
		if err != nil {
			return result, err
		}
		defer dir.Close()
		fileIndex, err := c.index.directory(c.ctx, c.store, p)
		if err != nil {
			return result, err
		}
		var children []objectfmt.Entry
		sum, err := c.walkChildren(p, dir, info, func(name, child string, childInfo fs.FileInfo) error {
			next, err := c.entryAt(dir, name, child, childInfo, fileIndex)
			if err == nil {
				children = append(children, next)
			}
			return err
		})
		if err != nil {
			return result, err
		}
		if runtime.GOOS != "windows" {
			c.directoryChecks = append(c.directoryChecks, directoryCheck{p, sum})
		}
		if err = c.index.finishDirectory(c.ctx, c.store, p, fileIndex); err != nil {
			return result, err
		}
		result.Digest, e = objectfmt.BuildDir(children, c.put)
	default:
		return result, fmt.Errorf("不支持的文件类型 %s: %s", p, info.Mode().Type())
	}
	return result, e
}
func (s *Store) Snapshot(ctx context.Context, unit, label string) (Snapshot, error) {
	return s.captureSnapshot(ctx, unit, label, false)
}

func (s *Store) captureSnapshot(ctx context.Context, unit, label string, scheduled bool) (Snapshot, error) {
	activity, lockErr := s.projectActivity(unit)
	if lockErr != nil {
		return Snapshot{}, lockErr
	}
	defer activity.Close()
	// Another project's removal can retire references while this capture has
	// computed an object but has not yet committed its references.
	captureLock, lockErr := s.beginSpoolCapture(ctx)
	if lockErr != nil {
		return Snapshot{}, lockErr
	}
	defer func() { captureLock.Close(); _ = s.collectDeferredSpool(ctx) }()
	var v Snapshot
	b, e := s.binding(ctx, unit)
	if e != nil {
		return v, e
	}
	if b.LocalFilesDeleted {
		return v, errLocalFilesDeleted
	}
	if e = checkCredentialPath(b.Path); e != nil {
		return v, e
	}
	var expectedMarker string
	if e = s.db.QueryRowContext(ctx, "SELECT identity FROM binding_roots WHERE unit=?", unit).Scan(&expectedMarker); e != nil {
		return v, fmt.Errorf("无法读取已登记目录身份，请重新运行 unit bind: %w", e)
	}
	dir, name := b.Path, "."
	if b.Profile == "file" {
		dir, name = filepath.Dir(dir), filepath.Base(dir)
	}
	if b.Profile == "codex" {
		st, err := os.Lstat(dir)
		if err != nil {
			return v, err
		}
		if !st.IsDir() || codexReparse(st) {
			return v, errors.New("Codex 来源根目录已被替换或链接")
		}
	}
	r, e := os.OpenRoot(dir)
	if e != nil {
		return v, e
	}
	defer r.Close()
	f, e := r.Open(".")
	if e != nil {
		return v, e
	}
	marker, e := directoryMarker(f)
	f.Close()
	if e != nil {
		return v, e
	}
	if marker != expectedMarker {
		return v, errors.New("来源目录已更换或卸载，请恢复原目录")
	}
	c := capture{ctx: ctx, store: s, root: r, refs: map[objectfmt.Ref]int{}}
	if scheduled && b.Base != nil {
		c.installed = []objectfmt.Ref{}
		c.deferredArchives = make(map[string]workspaceDigest)
	}
	if e = c.prepareArchives(b); e != nil {
		return v, e
	}
	ignored := defaultIgnored()
	if b.Profile == "project" {
		ignored, e = readIgnored(ctx, s.db, unit)
		if e != nil {
			return v, e
		}
		var isGit bool
		isGit, e = c.projectMembers(dir, ignored)
		if e != nil {
			return v, e
		}
		if isGit {
			if _, e = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO git_sources(unit) SELECT unit FROM bindings WHERE unit=? AND path=? AND generation=?", unit, b.Path, b.generation); e != nil {
				return v, e
			}
		} else {
			var previouslyGit bool
			if e = s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM git_sources WHERE unit=?)", unit).Scan(&previouslyGit); e != nil {
				return v, e
			}
			if previouslyGit {
				return v, errors.New("Git 工作区标记缺失，已停止采集，请恢复原工作区")
			}
		}
	}
	var item objectfmt.Entry
	if b.Profile == "codex" {
		item, e = c.codexTree()
	} else {
		c.index, e = s.loadCaptureIndex(ctx, b, r)
		if e == nil {
			item, e = c.entry(name)
		}
	}
	if e != nil {
		return v, e
	}
	if e = c.checkArchiveCoverage(); e != nil {
		return v, e
	}
	rootDigest := item.Digest
	if b.Profile == "file" {
		if item.Kind != "file" {
			return v, errors.New("文件来源已替换")
		}
		rootDigest, e = objectfmt.BuildDir([]objectfmt.Entry{item}, c.put)
		if e != nil {
			return v, e
		}
	}
	if e = c.verifyChecks(); e != nil {
		return v, e
	}
	if b.Profile == "codex" {
		st, err := os.Lstat(dir)
		if err != nil || !st.IsDir() || codexReparse(st) {
			return v, errors.New("采集期间 Codex 来源根目录已被替换或链接")
		}
	}
	if current, err := directoryMarkerAt(dir); err != nil || current != expectedMarker {
		return v, errors.New("采集期间来源目录已更换或卸载")
	}
	if e = c.index.save(ctx, s, true); e != nil {
		return v, e
	}
	if label == "" && b.Base != nil {
		old, err := s.snap(ctx, *b.Base)
		if err == nil {
			same, err := s.unchangedCapture(ctx, b, old, rootDigest, scheduled)
			if err != nil {
				return v, err
			}
			if same {
				if scheduled {
					return Snapshot{}, c.discardUnreferenced()
				}
				return old, nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return v, err
		}
	}
	for digest, expected := range c.deferredArchives {
		if e = c.collectArchiveRefs(digest, expected); e != nil {
			return v, e
		}
	}
	if e = c.collectReused(); e != nil {
		return v, e
	}
	v = Snapshot{newID(), unit, b.Base, rootDigest, label, time.Now().UTC().Format(time.RFC3339Nano), "queued"}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return v, e
	}
	defer tx.Rollback()
	currentIgnored, e := readIgnored(ctx, tx, unit)
	if e != nil {
		return v, e
	}
	if !ignored.equal(currentIgnored) {
		return v, errors.New("文件选择已变化，请重新保存快照")
	}
	rr, e := tx.ExecContext(ctx, "UPDATE bindings SET base=? WHERE unit=? AND base IS ? AND path=? AND profile=? AND generation=?", v.ID, unit, b.Base, b.Path, b.Profile, b.generation)
	if e != nil {
		return v, e
	}
	n, e := rr.RowsAffected()
	if e != nil {
		return v, e
	}
	if n != 1 {
		return v, errors.New("已有并发采集，请重试")
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO snapshots VALUES(?,?,?,?,?,?,?,?,NULL)", v.ID, unit, v.Parent, v.Root, v.Label, v.Captured, newID(), v.State); e != nil {
		return v, e
	}
	ins, e := tx.PrepareContext(ctx, "INSERT INTO snapshot_objects VALUES(?,?,?,?)")
	if e != nil {
		return v, e
	}
	defer ins.Close()
	for ref, sz := range c.refs {
		if _, e = ins.ExecContext(ctx, v.ID, ref.Kind, ref.Digest, sz); e != nil {
			return v, e
		}
	}
	return v, tx.Commit()
}

const snapCols = "id,unit,parent,root,label,captured,state"

func scanSnapshot(row interface{ Scan(...any) error }) (Snapshot, error) {
	var v Snapshot
	e := row.Scan(&v.ID, &v.Unit, &v.Parent, &v.Root, &v.Label, &v.Captured, &v.State)
	return v, e
}
func (s *Store) snap(ctx context.Context, id string) (Snapshot, error) {
	return scanSnapshot(s.db.QueryRowContext(ctx, "SELECT "+snapCols+" FROM snapshots WHERE id=?", id))
}
func (s *Store) Queue(ctx context.Context) ([]Snapshot, error) {
	return s.readSnapshots(ctx, "SELECT "+snapCols+" FROM snapshots ORDER BY captured,id")
}

func (s *Store) readSnapshots(ctx context.Context, query string) ([]Snapshot, error) {
	rows, e := s.db.QueryContext(ctx, query)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		v, e := scanSnapshot(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
