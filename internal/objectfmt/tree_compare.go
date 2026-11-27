package objectfmt

import (
	"context"
	"slices"
)

// SameTreeContents descends only into directories with different tree IDs. A scheduled capture
// follows names, bytes, execute permissions, empty directories and link targets.
// Filesystem timestamps and Finder's per-folder presentation metadata travel
// with the next real change. A file explicitly bound on its own is always data.
func SameTreeContents(ctx context.Context, previous, current string, skipFinder bool, previousLoad, currentLoad Load) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if previous == current {
		return true, nil
	}
	before, err := ReadDir(previous, previousLoad)
	if err != nil {
		return false, err
	}
	after, err := ReadDir(current, currentLoad)
	if err != nil {
		return false, err
	}
	if skipFinder {
		finderMetadata := func(e Entry) bool { return e.Name == ".DS_Store" && e.Kind == "file" }
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
		same, err := SameTreeContents(ctx, a.Digest, b.Digest, skipFinder, previousLoad, currentLoad)
		if err != nil || !same {
			return false, err
		}
	}
	return true, nil
}
