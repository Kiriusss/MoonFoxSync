package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// WatchToken identifies the capture basis for a resident filesystem observer.
// The caller also compares the snapshot ID to detect a changed baseline.
func (s *Store) WatchToken(ctx context.Context, b Binding) (string, error) {
	var selection string
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE((SELECT paths FROM ignored_selection WHERE unit=?),'')", b.Unit).Scan(&selection)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal([]string{b.generation, b.Path, b.Profile, selection})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
