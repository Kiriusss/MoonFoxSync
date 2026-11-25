//go:build darwin && cgo

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Open the original parent one component at a time, without traversing a link.
func workspaceParent(a WorkspaceFile) (*os.File, error) {
	dir := filepath.Dir(a.Source)
	fd, e := unix.Open("/", unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	for _, p := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		if p == "" {
			continue
		}
		next, err := unix.Openat(fd, p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), dir)
	m, e := directoryMarker(f)
	if e != nil || m != a.Directory {
		f.Close()
		return nil, errors.New("文件所在目录已移动或替换")
	}
	return f, nil
}
func safeOpenAt(d *os.File, name string, flags int) (*os.File, error) {
	if !localSegment(name) {
		return nil, errors.New("无效的文件名")
	}
	fd, e := unix.Openat(int(d.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), filepath.Join(d.Name(), name)), nil
}
func workspaceLink(d *os.File, name, target string) bool {
	buf := make([]byte, 4096)
	n, e := unix.Readlinkat(int(d.Fd()), name, buf)
	return e == nil && string(buf[:n]) == target
}
func archiveSwap(d *os.File, a, b string) error {
	return unix.RenameatxNp(int(d.Fd()), a, int(d.Fd()), b, unix.RENAME_SWAP)
}
func sameWorkVersion(a, b os.FileInfo) bool {
	if !os.SameFile(a, b) || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) {
		return false
	}
	return a.Sys().(*syscall.Stat_t).Ctimespec == b.Sys().(*syscall.Stat_t).Ctimespec
}
func verifyWorkFile(f *os.File, a WorkspaceFile) (os.FileInfo, error) {
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	sys := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || st.Size() != a.Size || sys.Nlink != 1 || int(sys.Uid) != os.Geteuid() || sys.Flags&(unix.UF_IMMUTABLE|unix.UF_APPEND|unix.SF_IMMUTABLE|unix.SF_APPEND) != 0 {
		return nil, errors.New("文件类型、所有者、硬链接或保护属性不适合原位置释放")
	}
	if _, e = f.Seek(0, 0); e != nil {
		return nil, e
	}
	h := sha256.New()
	n, e := io.Copy(h, f)
	if e != nil {
		return nil, e
	}
	now, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !sameWorkVersion(st, now) || n != a.Size || hex.EncodeToString(h.Sum(nil)) != a.SHA {
		return nil, errors.New("本机文件和已上传版本不同，保留原件；请先保存并上传新版本")
	}
	return st, nil
}
func (s *Store) metadataPath(a WorkspaceFile) string {
	return filepath.Join(s.Home, "workspace-metadata", a.Slot)
}
func (s *Store) saveMetadata(f *os.File, a WorkspaceFile) error {
	dst := s.metadataPath(a)
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	tmp, e := os.CreateTemp(filepath.Dir(dst), "metadata-")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if e = workspaceCopyMetadata(f, tmp); e != nil {
		return e
	}
	if e = tmp.Sync(); e != nil {
		return e
	}
	if e = os.Rename(tmp.Name(), dst); e != nil {
		return e
	}
	return syncDirectory(filepath.Dir(dst))
}
func (s *Store) macWorkspaceLock(unit string) (activityLocks, error) {
	g, e := s.localLock(false)
	if e != nil {
		return nil, e
	}
	p, e := s.projectLock(unit, true)
	if e != nil {
		g.Close()
		return nil, e
	}
	return activityLocks{g, p}, nil
}
func (s *Store) finishWorkspaceIntent(ctx context.Context, a WorkspaceFile) (WorkspaceFile, error) {
	// A failed hold release never impairs the reconstructed local file. Keep
	// the intent, so a later retry can remove the hold after network recovery.
	r, e := s.Remote(ctx)
	if e != nil {
		return a, e
	}
	e = r.call(ctx, "DELETE", "/units/"+a.Unit+"/holds/"+a.Slot, nil, nil, nil)
	var api *APIError
	if e != nil && !(errors.As(e, &api) && api.HTTP == 404) {
		return a, fmt.Errorf("原文件已在本机，等待解除 NAS 保留：%w", e)
	}
	if _, e = s.db.ExecContext(ctx, "DELETE FROM workspace_archives WHERE slot=?", a.Slot); e != nil {
		return a, e
	}
	_ = os.Remove(s.metadataPath(a))
	a.State = "local"
	return a, nil
}

func (s *Store) WorkspaceRelease(ctx context.Context, source, snapshot, sha string) (WorkspaceFile, error) {
	a, e := s.workspacePlan(ctx, source)
	if e != nil {
		return a, e
	}
	locks, e := s.macWorkspaceLock(a.Unit)
	if e != nil {
		return a, e
	}
	defer locks.Close()
	a, e = s.workspacePlan(ctx, source)
	if e != nil {
		return a, e
	}
	if a.Snapshot != snapshot || a.SHA != sha {
		return a, errors.New("预览所选版本或摘要已变化，请重新预览")
	}
	if a.State == "hydrating" {
		return a, errors.New("请先完成上次取回操作")
	}
	parent, e := workspaceParent(a)
	if e != nil {
		return a, e
	}
	defer parent.Close()
	volume := filepath.Join(s.Home, "workspace-volume")
	var vol unix.Statfs_t
	if e = unix.Statfs(volume, &vol); e != nil || unix.ByteSliceToString(vol.Fstypename[:]) != "webdav" || vol.Flags&unix.MNT_RDONLY == 0 || vol.Flags&unix.MNT_DONTBROWSE == 0 || unix.ByteSliceToString(vol.Mntonname[:]) != volume {
		return a, errors.New("原位置按需服务尚未挂载，请先在客户端启用")
	}
	name := filepath.Base(a.Source)
	if a.State == "online" {
		if !workspaceLink(parent, name, s.workspaceTarget(a)) {
			return a, errors.New("原路径已被另一个文件占用，请检查内容后重新选择")
		}
		return a, nil
	}
	if a.Slot == "" {
		f, e := safeOpenAt(parent, name, unix.O_RDONLY|unix.O_NONBLOCK)
		if e != nil {
			return a, e
		}
		defer f.Close()
		if _, e = verifyWorkFile(f, a); e != nil {
			return a, e
		}
		used, err := workspaceHeldByAnother(a.Source)
		if err != nil {
			return a, err
		}
		if used {
			return a, errors.New("文件仍被其他程序打开，请关闭后再试")
		}
		a.Slot = newID()
		a.State = "releasing"
		entry, e := json.Marshal(a.Entry)
		if e != nil {
			return a, e
		}
		_, e = s.db.ExecContext(ctx, "INSERT INTO workspace_archives("+workspaceColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", a.Slot, a.Unit, a.Snapshot, a.Member, a.Source, a.SHA, a.Size, a.Device, a.State, a.Generation, entry, a.Directory)
		if e != nil {
			return a, e
		}
	}
	if e = s.requireWorkspaceHold(ctx, a); e != nil {
		return a, e
	}
	stage := ".moonfox-" + a.Slot
	target := s.workspaceTarget(a)
	st, e := os.Stat(target)
	if e != nil || !st.Mode().IsRegular() || st.Size() != a.Size {
		return a, errors.New("按需服务还不能定位已保留版本；原件已保留，可重试或取回")
	}
	// Link and source change places atomically. The original inode is still
	// accessible under a private randomized name until its identity, bytes and
	// outstanding opens are checked again. No recursive removal is performed.
	if !workspaceLink(parent, name, target) {
		if !workspaceLink(parent, stage, target) {
			if e = unix.Symlinkat(target, int(parent.Fd()), stage); e != nil {
				return a, e
			}
		}
		if e = archiveSwap(parent, stage, name); e != nil {
			return a, e
		}
		if e = parent.Sync(); e != nil {
			return a, e
		}
	}
	rollback := func(reason error) (WorkspaceFile, error) {
		if workspaceLink(parent, name, target) {
			if err := archiveSwap(parent, stage, name); err == nil && workspaceLink(parent, stage, target) {
				if err = unix.Unlinkat(int(parent.Fd()), stage, 0); err == nil {
					if err = parent.Sync(); err == nil {
						_, cleanupErr := s.finishWorkspaceIntent(ctx, a)
						return a, errors.Join(reason, cleanupErr)
					}
				}
			}
		}
		return a, reason
	}
	orig, e := safeOpenAt(parent, stage, unix.O_RDONLY|unix.O_NONBLOCK)
	if e == nil {
		defer orig.Close()

		verified, e := verifyWorkFile(orig, a)
		if e != nil {
			return rollback(e)
		}
		if e = s.saveMetadata(orig, a); e != nil {
			return rollback(e)
		}
		used, err := workspaceHeldByAnother(filepath.Join(parent.Name(), stage))
		if err != nil {
			return rollback(err)
		}
		if used {
			return rollback(errors.New("文件仍被其他程序打开；原件已保留，关闭文件后可重试"))
		}
		st1, err := orig.Stat()
		if err != nil {
			return rollback(err)
		}
		st2, err := os.Lstat(filepath.Join(parent.Name(), stage))
		if err != nil || !os.SameFile(st1, st2) || !workspaceLink(parent, name, target) {
			return a, errors.New("原位置操作期间路径发生变化，隔离副本已保留")
		}
		if !sameWorkVersion(verified, st1) || !sameWorkVersion(st1, st2) {
			return rollback(errors.New("释放期间文件已被修改，已保留原件"))
		}
		if e = unix.Unlinkat(int(parent.Fd()), stage, 0); e != nil {
			return a, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return rollback(e)
	} else if _, e = os.Stat(s.metadataPath(a)); e != nil {
		return a, errors.New("原件与属性记录未经核对，请检查保留的副本")
	}
	if e = parent.Sync(); e != nil {
		return a, e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE workspace_archives SET phase='online' WHERE slot=?", a.Slot)
	a.State = "online"
	return a, e
}
func (s *Store) WorkspaceHydrate(ctx context.Context, source string) (WorkspaceFile, error) {
	a, e := s.sourceRecord(ctx, source)
	if e != nil {
		return a, e
	}
	locks, e := s.macWorkspaceLock(a.Unit)
	if e != nil {
		return a, e
	}
	defer locks.Close()
	a, e = s.sourceRecord(ctx, source)
	if e != nil {
		return a, e
	}
	d, e := workspaceParent(a)
	if e != nil {
		return a, e
	}
	defer d.Close()
	name := filepath.Base(a.Source)
	stage := ".moonfox-" + a.Slot
	target := s.workspaceTarget(a)

	// A user can remove the Finder link while its NAS retention is active.
	// Recreate it only in the recorded parent and only if the name is vacant.
	if _, err := os.Lstat(filepath.Join(d.Name(), name)); errors.Is(err, os.ErrNotExist) {
		if err = unix.Symlinkat(target, int(d.Fd()), name); err != nil {
			return a, err
		}
		if err = d.Sync(); err != nil {
			return a, err
		}
	}
	if !workspaceLink(d, name, target) {
		// Recover only an intact source left by a failed release, or our own
		// completed swap. An unrelated replacement must be kept untouched.
		f, err := safeOpenAt(d, name, unix.O_RDONLY|unix.O_NONBLOCK)
		if err != nil {
			return a, err
		}
		defer f.Close()
		if _, err = verifyWorkFile(f, a); err != nil {
			return a, fmt.Errorf("原位置已有文件。如需取回备份，请先将现有文件移到其他位置再重试：%w", err)
		}
		if workspaceLink(d, stage, target) {
			if err = unix.Unlinkat(int(d.Fd()), stage, 0); err != nil {
				return a, err
			}
		}
		return s.finishWorkspaceIntent(ctx, a)
	}
	if a.State == "releasing" {
		// A terminated release can still have the full original inode staged.
		if f, err := safeOpenAt(d, stage, unix.O_RDONLY|unix.O_NONBLOCK); err == nil {
			f.Close()
			if e = archiveSwap(d, stage, name); e != nil {
				return a, e
			}
			if workspaceLink(d, stage, target) {
				_ = unix.Unlinkat(int(d.Fd()), stage, 0)
			}
			if e = d.Sync(); e != nil {
				return a, e
			}
			return s.finishWorkspaceIntent(ctx, a)
		} else if !errors.Is(err, os.ErrNotExist) {
			return a, err
		}
	}
	_, e = s.db.ExecContext(ctx, "UPDATE workspace_archives SET phase='hydrating' WHERE slot=?", a.Slot)
	if e != nil {
		return a, e
	}
	// Create local bytes from the pinned NAS snapshot even if the Finder
	// mount is unavailable. Never overwrite a file someone created meanwhile.
	r, e := s.Remote(ctx)
	if e != nil {
		return a, e
	}
	alreadyDownloaded := false
	tmp, e := safeOpenAt(d, stage, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if e != nil {
		if !errors.Is(e, os.ErrExist) {
			return a, e
		}
		tmp, e = safeOpenAt(d, stage, unix.O_RDWR|unix.O_NONBLOCK)
		if e != nil {
			return a, e
		}
		valid, err := tmp.Stat()
		if err != nil || !valid.Mode().IsRegular() || valid.Sys().(*syscall.Stat_t).Nlink != 1 {
			tmp.Close()
			return a, errors.New("恢复暂存文件身份改变")
		}
		// Only reuse a verified completed copy. Preserve unexpected contents.
		if _, err = verifyWorkFile(tmp, a); err != nil {
			tmp.Close()
			recovery := ".moonfox-recovery-" + newID()
			if err = unix.RenameatxNp(int(d.Fd()), stage, int(d.Fd()), recovery, unix.RENAME_EXCL); err != nil {
				return a, err
			}
			if err = d.Sync(); err != nil {
				return a, err
			}
			tmp, e = safeOpenAt(d, stage, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
			if e != nil {
				return a, e
			}
		} else {
			alreadyDownloaded = true
		}
	}
	if !alreadyDownloaded {
		tr := startTransfer(s.Home, "download", a.Unit, a.Snapshot)
		limit, err := r.downloadLimit(ctx)
		if err == nil {
			err = r.downloadTo(ctx, tr, a.Unit, a.Snapshot, a.Member, tmp, a.Size, a.SHA, limit)
		}
		tr.close()
		if err != nil {
			current, statErr := os.Lstat(filepath.Join(d.Name(), stage))
			owned, ownErr := tmp.Stat()
			if ownErr == nil && statErr == nil && os.SameFile(owned, current) {
				_ = unix.Unlinkat(int(d.Fd()), stage, 0)
			}
			tmp.Close()
			return a, err
		}
	}
	defer tmp.Close()
	if _, e = verifyWorkFile(tmp, a); e != nil {
		return a, e
	}
	meta, e := os.Open(s.metadataPath(a))
	if e == nil {
		defer meta.Close()
		if e = workspaceCopyMetadata(meta, tmp); e != nil {
			return a, e
		}
	} else if errors.Is(e, os.ErrNotExist) {
		// A user may have moved the original before the first release finished.
		// Only restore the snapshot metadata that we know, with owner-only access.
		if a.Entry.Executable {
			if err := tmp.Chmod(0700); err != nil {
				return a, err
			}
		}
		if a.Entry.MtimeNS > 0 {
			t := unix.NsecToTimeval(a.Entry.MtimeNS)
			if err := unix.Futimes(int(tmp.Fd()), []unix.Timeval{t, t}); err != nil {
				return a, err
			}
		}
	} else {
		return a, e
	}
	if e = tmp.Sync(); e != nil {
		return a, e
	}
	if !workspaceLink(d, name, target) {
		return a, errors.New("下载期间原路径被替换，两份内容均已保留")
	}
	if e = archiveSwap(d, stage, name); e != nil {
		return a, e
	}
	if !workspaceLink(d, stage, target) {
		_ = archiveSwap(d, stage, name)
		return a, errors.New("原路径出现新文件，已保留")
	}
	if e = d.Sync(); e != nil {
		return a, e
	}
	if e = unix.Unlinkat(int(d.Fd()), stage, 0); e != nil {
		return a, e
	}
	if e = d.Sync(); e != nil {
		return a, e
	}
	return s.finishWorkspaceIntent(ctx, a)
}
