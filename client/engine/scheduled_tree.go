package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"

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
	var missingBaseline bool
	loadPrevious := func(ref objectfmt.Ref) ([]byte, error) {
		raw, err := s.readSpoolBytes(ref)
		missingBaseline = errors.Is(err, os.ErrNotExist)
		return raw, err
	}
	same, err := objectfmt.SameTreeContents(ctx, base, root, binding.Profile == "project", loadPrevious, s.readSpoolBytes)
	if missingBaseline && errors.Is(err, os.ErrNotExist) {
		// A remote checkout need not have the historical tree in its local spool.
		return false, nil
	}
	return same, err
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
