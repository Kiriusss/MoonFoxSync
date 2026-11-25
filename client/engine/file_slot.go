package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// All CLI and agent processes using the same client state share these locks.
// Keep the filenames stable: unlinking an unlocked file can split contenders
// between different inodes. Process exit releases ownership on both platforms.
func acquireFileSlot(ctx context.Context, home, direction string, limit int) (func(), error) {
	if limit < 1 || limit > maxFileTransfers {
		return nil, errors.New("并行文件数须为 1 至 50")
	}
	if home == "" {
		return func() {}, ctx.Err()
	}
	dir := filepath.Join(home, "transfer-slots", direction)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for i := 0; i < limit; i++ {
			f, err := cacheLock(filepath.Join(dir, strconv.Itoa(i)))
			if err == nil {
				return func() { f.Close() }, nil
			}
			if !lockBusy(err) {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
}
