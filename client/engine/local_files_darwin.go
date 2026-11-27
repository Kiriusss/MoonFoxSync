//go:build darwin && cgo

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"kirius.local/kiriusdrive/internal/objectfmt"
)

type localDeletion struct {
	ctx          context.Context
	s            *Store
	p            *workspacePlanner
	remote       *Remote
	roots        map[string]string
	readSnapshot string
	reserved     map[string]bool
	out          LocalFileDeleteResult
}

// Written before moving an item aside. All destructive steps use a private,
// randomly named entry in the original parent, identified by vnode and birth time.
type deleteIntent struct {
	Member, Slot, Snapshot, Directory, Stage, Inode string
}

func inodeKey(st os.FileInfo) string {
	v := st.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%x:%x:%x:%x", v.Dev, v.Ino, v.Birthtimespec.Sec, v.Birthtimespec.Nsec)
}

func (s *Store) DeleteLocalFiles(ctx context.Context, unit, source string) (LocalFileDeleteResult, error) {
	out := LocalFileDeleteResult{Details: []string{}}
	if err := validID(unit); err != nil {
		return out, err
	}
	locks, err := s.macWorkspaceLock(unit)
	if err != nil {
		return out, err
	}
	defer locks.Close()
	b, err := s.binding(ctx, unit)
	if err != nil {
		return out, err
	}
	if source == "" || b.Path != source || (b.Profile != "project" && b.Profile != "file") {
		return out, errors.New("项目绑定已改变或不支持删除，请重新打开项目菜单")
	}
	canonical, err := canonicalWorkspacePath(source)
	if err != nil || canonical != source {
		return out, errors.New("原路径已移动或发生变化")
	}
	if err = checkCredentialPath(source); err != nil {
		return out, err
	}
	p, err := s.workspacePlannerFor(ctx, source, b.Profile == "project", false)
	if err != nil {
		return out, err
	}
	if p.binding.Unit != unit || p.binding.generation != b.generation {
		return out, errors.New("项目绑定已改变")
	}
	if p.snapshot.ID == "" && len(p.archives) == 0 {
		return out, errors.New("还没有已上传的备份，请先同步到 NAS")
	}
	out.Paused = b.LocalFilesDeleted
	r, err := s.Remote(ctx)
	if err != nil {
		return out, err
	}
	d := localDeletion{ctx: ctx, s: s, p: p, remote: r, roots: map[string]string{}, reserved: map[string]bool{}, out: out}
	p.index.load = d.loadMetadata
	// Cache the authenticated receipt for each referenced immutable snapshot,
	// including older versions used by existing on-demand files. No data download.
	if p.snapshot.ID != "" {
		if err = d.checkSnapshot(p.snapshot.ID); err != nil {
			return out, err
		}
	}
	for _, a := range p.archives {
		if a.State == "online" {
			if err = d.checkSnapshot(a.Snapshot); err != nil {
				return out, err
			}
		}
	}
	bindings, err := s.Bindings(ctx)
	if err != nil {
		return out, err
	}
	for _, other := range bindings {
		if other.Unit != unit && other.Path != "" && inside(p.root, other.Path) {
			d.reserved[other.Path] = true
		}
	}
	if err = d.recoverIntent(); err != nil {
		return d.out, err
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return d.out, err
	}
	defer root.Close()
	if b.Profile == "file" {
		info, err := root.Lstat(filepath.Base(source))
		if errors.Is(err, os.ErrNotExist) {
			return d.out, nil
		}
		if err != nil {
			return d.out, err
		}
		err = d.removeOne(source, info)
		return d.endSweep(err)
	}
	err = fs.WalkDir(root.FS(), ".", func(rel string, item fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(p.root, filepath.FromSlash(rel))
		if walkErr != nil {
			if rel == "." {
				return walkErr
			}
			d.out.skip(rel, walkErr)
			return nil
		}
		if rel == "." {
			return nil
		}
		if d.reserved[path] || strings.EqualFold(item.Name(), ".git") {
			d.out.skip(rel, errors.New("保留独立绑定或 Git 元数据"))
			if item.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if item.IsDir() {
			_, err := root.Lstat(filepath.Join(filepath.FromSlash(rel), ".git"))
			if err == nil {
				d.out.skip(rel, errors.New("保留嵌套 Git 项目"))
				return fs.SkipDir
			}
			if !errors.Is(err, os.ErrNotExist) {
				d.out.skip(rel, err)
				return fs.SkipDir
			}
			return nil
		}
		info, err := item.Info()
		if err != nil {
			d.out.skip(rel, err)
			return nil
		}
		return d.removeOne(path, info)
	})
	return d.endSweep(err)
}

// A read-only parent, an ACL, or a last-moment edit can prevent the rename
// after the first successful verification. Restore the original schedule only
// after a complete no-op pass, never on an interrupted or ambiguous commit.
func (d *localDeletion) endSweep(err error) (LocalFileDeleteResult, error) {
	b := d.p.binding
	if err != nil || b.LocalFilesDeleted || !d.out.Paused || d.out.Removed != 0 {
		return d.out, err
	}
	result, err := d.s.db.ExecContext(d.ctx, `DELETE FROM local_deleted_sources WHERE unit=? AND generation=?
 AND NOT EXISTS(SELECT 1 FROM local_delete_intents WHERE unit=?)`, b.Unit, b.generation, b.Unit)
	if err != nil {
		return d.out, err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 1 {
		d.out.Paused = false
	}
	return d.out, err
}

func (d *localDeletion) checkSnapshot(id string) error {
	if _, ok := d.roots[id]; ok {
		return nil
	}
	if err := validID(id); err != nil {
		return err
	}
	snap, err := d.s.snap(d.ctx, id)
	remoteBase := errors.Is(err, sql.ErrNoRows) && id == d.p.snapshot.ID && d.p.snapshot.Root == ""
	if err != nil && !remoteBase {
		return err
	}
	unit := d.p.binding.Unit
	if !remoteBase && (snap.Unit != unit || (snap.State != "published" && snap.State != "fork")) {
		return errors.New("找不到本项目已发布的版本")
	}
	var receipt Receipt
	if err = d.remote.call(d.ctx, "GET", "/units/"+unit+"/snapshots/"+id, nil, nil, &receipt); err != nil {
		return fmt.Errorf("无法从 NAS 确认已上传的版本：%w", err)
	}
	if receipt.Unit != unit || receipt.Snapshot != id || len(receipt.Root) != 64 || (receipt.Outcome != "published" && receipt.Outcome != "fork") || (!remoteBase && (receipt.Root != snap.Root || receipt.Outcome != snap.State)) {
		return errors.New("NAS 版本与本机保存的发布记录不一致")
	}
	d.roots[id] = receipt.Root
	if remoteBase {
		d.p.snapshot.Root = receipt.Root
		d.p.snapshot.State = receipt.Outcome
	}
	return nil
}

// Checkout intentionally does not create a second local object cache. Read
// missing manifests from the exact verified snapshot; never request a chunk.
func (d *localDeletion) loadMetadata(ref objectfmt.Ref) ([]byte, error) {
	if ref.Kind != "tree" && ref.Kind != "file" {
		return nil, errors.New("只支持读取快照清单")
	}
	// The workspace index decodes this page with objectfmt, including digest verification.
	data, err := d.s.readSpoolBytes(ref)
	if !errors.Is(err, os.ErrNotExist) {
		return data, err
	}
	if _, ok := d.roots[d.readSnapshot]; !ok {
		return nil, errors.New("NAS 版本尚未核对")
	}
	q := url.Values{"kind": {ref.Kind}}
	path := "/units/" + d.p.binding.Unit + "/snapshots/" + d.readSnapshot + "/objects/" + ref.Digest
	resp, err := d.remote.request(d.ctx, "GET", path, q, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, objectfmt.ChunkSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > objectfmt.ChunkSize || objectfmt.Digest(data) != ref.Digest {
		return nil, errors.New("NAS 快照清单校验失败")
	}
	return data, nil
}

// Recheck the selected subtree and all intervening directories before using
// the pinned parent descriptor. A newly nested repository belongs to itself.
func (d *localDeletion) parent(a WorkspaceFile) (*os.File, error) {
	root := d.p.root
	if !validMember(a.Member) || a.Source != filepath.Join(root, filepath.FromSlash(a.Member)) || (d.p.binding.Profile == "file" && a.Source != d.p.binding.Path) {
		return nil, errors.New("文件不属于所选绑定")
	}
	marker, e := directoryMarkerAt(root)
	if e != nil || marker != d.p.markers[root] {
		return nil, errors.New("项目根目录已被替换")
	}
	for cur := filepath.Dir(a.Source); cur != root; cur = filepath.Dir(cur) {
		if !inside(root, cur) || d.reserved[cur] {
			return nil, errors.New("文件不属于当前项目")
		}
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil || !os.IsNotExist(err) {
			return nil, errors.New("上级目录已成为嵌套 Git 项目或无法读取")
		}
	}
	if d.reserved[a.Source] {
		return nil, errors.New("此文件属于另一个绑定")
	}
	return workspaceParent(a)
}

func (d *localDeletion) reference(i deleteIntent) (WorkspaceFile, error) {
	var a WorkspaceFile
	if !validMember(i.Member) || validID(i.Snapshot) != nil {
		return a, errors.New("无效的删除记录")
	}
	d.readSnapshot = i.Snapshot
	e, err := d.p.index.entry(d.roots[i.Snapshot], i.Member)
	if err != nil {
		return a, err
	}
	data, err := d.p.index.content(e.Digest)
	if err != nil {
		return a, err
	}
	a = WorkspaceFile{Unit: d.p.binding.Unit, Source: filepath.Join(d.p.root, filepath.FromSlash(i.Member)), Member: i.Member, Snapshot: i.Snapshot, SHA: data.sha, Size: data.size, Entry: e, Directory: i.Directory, Slot: i.Slot}
	if i.Slot != "" {
		arch, ok := d.p.archives[a.Source]
		if !ok || validID(i.Slot) != nil || arch.Slot != i.Slot || arch.State != "online" || arch.Member != a.Member || arch.Snapshot != i.Snapshot || arch.Unit != a.Unit || arch.Directory != a.Directory || arch.Generation != d.p.binding.generation || arch.Device != d.p.device || arch.SHA != a.SHA || arch.Size != a.Size {
			return a, errors.New("原位置按需记录与 NAS 版本不符")
		}
	}
	return a, nil
}

func checkResourceFork(f *os.File) error {
	n, err := unix.Fgetxattr(int(f.Fd()), "com.apple.ResourceFork", nil)
	if err != nil && !errors.Is(err, unix.ENOATTR) && !errors.Is(err, unix.ENOTSUP) {
		return err
	}
	if n > 0 {
		return errors.New("保留带有资源分叉的文件")
	}
	return nil
}
func verifyLocalData(parent *os.File, name string, a WorkspaceFile) (os.FileInfo, error) {
	f, err := safeOpenAt(parent, name, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err = checkResourceFork(f); err != nil {
		return nil, err
	}
	st, err := verifyWorkFile(f, a)
	if err != nil {
		return nil, err
	}
	if (st.Mode().Perm()&0111 != 0) != a.Entry.Executable {
		return nil, errors.New("执行权限与已上传版本不同")
	}
	used, err := workspaceHeldByAnother(filepath.Join(parent.Name(), name))
	if err != nil {
		return nil, err
	}
	if used {
		return nil, errors.New("文件正在使用")
	}
	now, err := os.Lstat(filepath.Join(parent.Name(), name))
	if err != nil {
		return nil, err
	}
	if !sameWorkVersion(st, now) {
		return nil, errors.New("检查期间内容发生变化")
	}
	return st, nil
}

func (d *localDeletion) verify(parent *os.File, name string, a WorkspaceFile, inode string) error {
	var info os.FileInfo
	var err error
	if a.Slot != "" {
		// The old release flow parked Finder metadata, including resource forks,
		// in a local sidecar. Never discard its unique data when deleting a link.
		meta, openErr := os.Open(d.s.metadataPath(a))
		if openErr == nil {
			defer meta.Close()
			if e := checkResourceFork(meta); e != nil {
				return e
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			return openErr
		}
		if !workspaceLink(parent, name, d.s.workspaceTarget(a)) {
			return errors.New("按需链接已被替换")
		}
		info, err = os.Lstat(filepath.Join(parent.Name(), name))
	} else {
		info, err = verifyLocalData(parent, name, a)
	}
	if err != nil {
		return err
	}
	if inodeKey(info) != inode {
		return errors.New("路径已被其他文件占用")
	}
	return nil
}
func (d *localDeletion) removeOne(path string, info os.FileInfo) error {
	p := d.p
	d.readSnapshot = p.snapshot.ID
	a, err := p.file(path, info)
	if err != nil {
		d.out.skip(path, err)
		return nil
	}
	i := deleteIntent{Member: a.Member, Slot: a.Slot, Snapshot: a.Snapshot, Directory: a.Directory, Stage: ".moonfox-delete-" + newID(), Inode: inodeKey(info)}
	if a.State != "local" && a.State != "online" {
		d.out.skip(path, errors.New("存在待完成的取回或释放操作"))
		return nil
	}
	a, err = d.reference(i)
	if err != nil {
		d.out.skip(path, err)
		return nil
	}
	parent, err := d.parent(a)
	if err != nil {
		d.out.skip(path, err)
		return nil
	}
	defer parent.Close()
	name := filepath.Base(path)
	// Check before changing the project state. A wholly modified/open folder
	// must still synchronize normally after a no-op deletion attempt.
	if err = d.verify(parent, name, a, i.Inode); err != nil {
		d.out.skip(path, err)
		return nil
	}
	if !d.out.Paused {
		if err = d.s.pauseLocalDeletion(d.ctx, p.binding); err != nil {
			return err
		}
		d.out.Paused = true
	}
	raw, err := json.Marshal(i)
	if err != nil {
		return err
	}
	_, err = d.s.db.ExecContext(d.ctx, "INSERT INTO local_delete_intents(unit,generation,intent) VALUES(?,?,?)", a.Unit, p.binding.generation, raw)
	if err != nil {
		return err
	}
	fd := int(parent.Fd())
	if err = unix.RenameatxNp(fd, name, fd, i.Stage, unix.RENAME_EXCL); err != nil {
		d.out.skip(path, err)
		return d.clearIntent(a, false)
	}
	if err = parent.Sync(); err != nil {
		return err
	}
	return d.finishStaged(parent, i, a)
}

func (d *localDeletion) clearIntent(a WorkspaceFile, removed bool) error {
	tx, err := d.s.db.BeginTx(d.ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if removed && a.Slot != "" {
		if _, err = tx.Exec("DELETE FROM workspace_archives WHERE unit=? AND slot=? AND generation=?", a.Unit, a.Slot, d.p.binding.generation); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM local_delete_intents WHERE unit=? AND generation=?", a.Unit, d.p.binding.generation); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if removed {
		d.out.Removed++
		if a.Slot != "" {
			delete(d.p.archives, a.Source)
			_ = os.Remove(d.s.metadataPath(a))
		}
	}
	return nil
}

func (d *localDeletion) recoverIntent() error {
	var raw []byte
	var gen string
	err := d.s.db.QueryRowContext(d.ctx, "SELECT generation,intent FROM local_delete_intents WHERE unit=?", d.p.binding.Unit).Scan(&gen, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var i deleteIntent
	if err = json.Unmarshal(raw, &i); err != nil {
		return err
	}
	if gen != d.p.binding.generation || !strings.HasPrefix(i.Stage, ".moonfox-delete-") || validID(strings.TrimPrefix(i.Stage, ".moonfox-delete-")) != nil {
		return errors.New("未完成操作与当前绑定不一致")
	}
	if err = d.checkSnapshot(i.Snapshot); err != nil {
		return err
	}
	a, err := d.reference(i)
	if err != nil {
		return err
	}
	parent, err := d.parent(a)
	if err != nil {
		return err
	}
	defer parent.Close()
	d.out.Paused = true
	if _, err = os.Lstat(filepath.Join(parent.Name(), i.Stage)); errors.Is(err, os.ErrNotExist) {
		// Rename either never happened or the unlink completed before a crash.
		current, statErr := os.Lstat(a.Source)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		removed := errors.Is(statErr, os.ErrNotExist) || inodeKey(current) != i.Inode
		return d.clearIntent(a, removed)
	} else if err != nil {
		return err
	}
	return d.finishStaged(parent, i, a)
}

func (d *localDeletion) finishStaged(parent *os.File, i deleteIntent, a WorkspaceFile) error {
	fd := int(parent.Fd())
	if err := d.verify(parent, i.Stage, a, i.Inode); err != nil {
		// Return even a swapped-in item's bytes, without overwriting anything newly
		// created at the original name. On conflict the journal preserves both.
		if move := unix.RenameatxNp(fd, i.Stage, fd, filepath.Base(a.Source), unix.RENAME_EXCL); move != nil {
			return fmt.Errorf("已保留暂存文件 %s，先移开冲突文件并重试：%w", filepath.Join(parent.Name(), i.Stage), move)
		}
		if errSync := parent.Sync(); errSync != nil {
			return errSync
		}
		d.out.skip(a.Source, err)
		return d.clearIntent(a, false)
	}
	if err := unix.Unlinkat(fd, i.Stage, 0); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	return d.clearIntent(a, true)
}
