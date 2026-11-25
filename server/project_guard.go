package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"kirius.local/kiriusdrive/internal/objectfmt"
)

// The object wire format normally includes mtimes. Independent clones commonly
// have different mtimes, so compare canonical directory entries with time zeroed.
// File object digests retain all original bytes and executable flags stay in the tree.
func (s *Store) contentFingerprint(ctx context.Context, root string) (string, error) {
	return s.contentFingerprintMemo(ctx, root, map[string]string{})
}
func (s *Store) contentFingerprintMemo(ctx context.Context, root string, memo map[string]string) (string, error) {
	var walk func(string, int) (string, error)
	walk = func(tree string, depth int) (string, error) {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		if id, ok := memo[tree]; ok {
			return id, nil
		}
		if depth > 512 {
			return "", errors.New("project directory depth exceeds limit")
		}
		entries, e := objectfmt.ReadDir(tree, s.read)
		if e != nil {
			return "", e
		}
		for i := range entries {
			entries[i].MtimeNS = 0
			if entries[i].Kind == "directory" {
				entries[i].Digest, e = walk(entries[i].Digest, depth+1)
				if e != nil {
					return "", e
				}
			}
		}
		id, e := objectfmt.BuildDir(entries, func(objectfmt.Ref, []byte) error { return nil })
		if e == nil {
			memo[tree] = id
		}
		return id, e
	}
	return walk(root, 0)
}

// For a shared project, a foreign head alone is insufficient evidence of a
// complete baseline. Legacy clients without a proof may publish an equivalent
// tree, or save their divergent work as a fork. Their bytes remain readable.
func (s *Store) mayAdvanceShared(ctx context.Context, tx *sql.Tx, unit, dev string, d draft) (bool, error) {
	if d.Base == nil {
		return true, nil
	}
	var shared bool
	if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM projects WHERE unit=?)", unit).Scan(&shared); e != nil {
		return false, e
	}
	if !shared {
		return true, nil
	}
	var author, root string
	if e := tx.QueryRowContext(ctx, `SELECT json_extract(record,'$.deviceId'),root FROM snapshots WHERE unit=? AND id=?`, unit, *d.Base).Scan(&author, &root); e != nil {
		return false, e
	}
	if (author == dev && d.BaseContentDigest == "") || root == d.Root {
		return true, nil
	}
	// Shared subtrees in the two snapshots normalize to the same digest.
	memo := make(map[string]string)
	expected, e := s.contentFingerprintMemo(ctx, root, memo)
	if e != nil {
		return false, e
	}
	if expected == d.BaseContentDigest {
		return true, nil
	}
	actual, e := s.contentFingerprintMemo(ctx, d.Root, memo)
	return actual == expected, e
}

func (s *Store) snapshotWithFingerprint(ctx context.Context, record []byte) (map[string]any, error) {
	var fields map[string]any
	if e := json.Unmarshal(record, &fields); e != nil {
		return nil, e
	}
	root, _ := fields["rootDigest"].(string)
	digest, e := s.contentFingerprint(ctx, root)
	if e != nil {
		return nil, e
	}
	fields["contentDigest"] = digest
	return fields, nil
}
