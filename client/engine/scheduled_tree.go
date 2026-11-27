package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func (s *Store) unchangedCapture(ctx context.Context, binding Binding, old Snapshot, root string, scheduled bool) (bool, error) {
	if old.Root == root {
		return true, nil
	}
	// Checkout can assign different local timestamps from those in the remote tree.
	var restoredRoot string
	err := s.db.QueryRowContext(ctx, "SELECT local_root FROM checkout_base WHERE unit=? AND snapshot=?", binding.Unit, old.ID).Scan(&restoredRoot)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if root == restoredRoot {
		return true, nil
	}
	if !scheduled || binding.Profile == "codex" {
		return false, nil
	}
	base := old.Root
	if restoredRoot != "" {
		base = restoredRoot
	}
	return sameScheduledTree(ctx, base, root, binding.Profile == "project", s.readSpoolBytes)
}

// Descend only into directories with different tree IDs. A scheduled capture
// follows names, bytes, execute permissions, empty directories and link targets.
// Filesystem timestamps and Finder's per-folder presentation metadata travel
// with the next real change. A file explicitly bound on its own is always data.
func sameScheduledTree(ctx context.Context, previous, current string, skipFinder bool, load objectfmt.Load) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if previous == current {
		return true, nil
	}
	before, err := objectfmt.ReadDir(previous, load)
	if errors.Is(err, os.ErrNotExist) {
		// A remote checkout need not have a local copy of the historical tree.
		// Without a baseline, keep the capture instead of dismissing a change.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	after, err := objectfmt.ReadDir(current, load)
	if err != nil {
		return false, err
	}
	if skipFinder {
		finderMetadata := func(e objectfmt.Entry) bool { return e.Name == ".DS_Store" && e.Kind == "file" }
		before = slices.DeleteFunc(before, finderMetadata)
		after = slices.DeleteFunc(after, finderMetadata)
	}
	if len(before) != len(after) {
		return false, nil
	}
	for i, a := range before {
		b := after[i]
		if a.Name != b.Name || a.Kind != b.Kind || a.Executable != b.Executable || a.Target != b.Target {
			return false, nil
		}
		if a.Digest == b.Digest {
			continue
		}
		if a.Kind != "directory" {
			return false, nil
		}
		same, err := sameScheduledTree(ctx, a.Digest, b.Digest, skipFinder, load)
		if err != nil || !same {
			return false, err
		}
	}
	return true, nil
}

// Retire speculative objects through the same reference-checked sweep as
// library removal. An overlapping capture may still commit these objects.
func (c *capture) discardUnreferenced() error {
	if len(c.installed) == 0 {
		return nil
	}
	tx, err := c.store.db.BeginTx(c.ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insert, err := tx.PrepareContext(c.ctx, "INSERT OR IGNORE INTO deferred_spool_gc VALUES(?,?)")
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, ref := range c.installed {
		if _, err := insert.ExecContext(c.ctx, ref.Kind, ref.Digest); err != nil {
			return err
		}
	}
	return tx.Commit()
}
