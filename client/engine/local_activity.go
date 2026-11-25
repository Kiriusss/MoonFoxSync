package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Old workers hold only local-maintenance.lock. Establish the new protocol
// between operations so workers surviving an application upgrade finish safely.
func (s *Store) scopedMaintenanceReady() bool {
	marker := filepath.Join(s.Home, "project-locks-v1")
	if info, err := os.Stat(marker); err == nil {
		return info.IsDir()
	}
	gate, err := lockFile(filepath.Join(s.Home, "local-maintenance.lock"), true)
	if err != nil {
		return false
	}
	defer gate.Close()
	return os.MkdirAll(marker, 0700) == nil
}

type activityLocks []*os.File

func (locks activityLocks) Close() {
	for i := len(locks) - 1; i >= 0; i-- {
		_ = locks[i].Close()
	}
}

var errProjectInUse = errors.New("此项目正在使用，请在该项目的当前任务结束后重试")
var errProjectRemoving = errors.New("此项目正在清理，请稍后重试")

// Filenames are stable even when a unit is removed and later re-imported.
// Never unlink an advisory lock: another process may already have its inode open.
func (s *Store) projectLock(unit string, exclusive bool) (*os.File, error) {
	if err := validID(unit); err != nil {
		return nil, err
	}
	f, err := lockFile(filepath.Join(s.Home, "activity-"+unit+".lock"), exclusive)
	if lockBusy(err) {
		if exclusive {
			return nil, errProjectInUse
		}
		return nil, errProjectRemoving
	}
	return f, err
}

func (s *Store) projectActivity(units ...string) (activityLocks, error) {
	global, err := s.localLock(false)
	if err != nil {
		return nil, err
	}
	locks := activityLocks{global}
	for _, unit := range units {
		f, err := s.projectLock(unit, false)
		if err != nil {
			locks.Close()
			return nil, err
		}
		locks = append(locks, f)
	}
	return locks, nil
}

// Capture holds this shared lock from its first spool lookup through SQLite
// commit. Only physical collection waits, other projects can forget records.
func (s *Store) beginSpoolCapture(ctx context.Context) (*os.File, error) {
	path := filepath.Join(s.Home, "spool-capture.lock")
	for {
		f, err := lockFile(path, false)
		if !lockBusy(err) {
			return f, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func (s *Store) collectDeferredSpool(ctx context.Context) error {
	var pending bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM deferred_spool_gc)").Scan(&pending); err != nil || !pending {
		return err
	}
	gc, err := lockFile(filepath.Join(s.Home, "spool-capture.lock"), true)
	if lockBusy(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer gc.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Reserve the writer before inspecting references, including on retries.
	if _, err = tx.ExecContext(ctx, "DELETE FROM deferred_spool_gc WHERE 0"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT kind,digest FROM deferred_spool_gc")
	if err != nil {
		return err
	}
	var refs [][2]string
	for rows.Next() {
		var ref [2]string
		if err = rows.Scan(&ref[0], &ref[1]); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err = s.removeSpoolObject(ctx, tx, ref[0], ref[1]); err != nil {
			return fmt.Errorf("清理本机未引用对象失败: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM deferred_spool_gc"); err != nil {
		return err
	}
	return tx.Commit()
}
