package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestScheduledChangesIgnoreFinderAndTimestamps(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the original Binding to cover another process advancing its base.
	capture := func(changed bool) *Snapshot {
		t.Helper()
		if _, err := f.s.db.Exec("UPDATE schedules SET last_scan=0 WHERE unit=?", f.unit); err != nil {
			t.Fatal(err)
		}
		v, completed, err := f.s.CaptureScheduled(ctx, b, AutoPolicy{})
		if err != nil || !completed || (v != nil) != changed {
			t.Fatalf("changed=%t result=%v completed=%t err=%v", changed, v, completed, err)
		}
		return v
	}
	dir := filepath.Join(f.source, "folder")
	file := filepath.Join(dir, "data")
	finder := filepath.Join(dir, ".DS_Store")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(file, "before")
	initial := capture(true)
	objectCount := func() int {
		t.Helper()
		n := 0
		err := filepath.WalkDir(filepath.Join(f.s.Home, "spool"), func(_ string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				n++
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	initialObjects := objectCount()
	capture(false)
	for _, path := range []string{file, dir} {
		otherTime := time.Unix(946684800, 0)
		if err := os.Chtimes(path, otherTime, otherTime); err != nil {
			t.Fatal(err)
		}
	}
	capture(false)
	write(finder, "presentation")
	capture(false)
	write(finder, "new presentation")
	capture(false)
	if n := objectCount(); n != initialObjects {
		t.Fatalf("unchanged scan leaked spool files: got %d, want %d", n, initialObjects)
	}
	write(file, "edited")
	next := capture(true)
	if next.Parent == nil || *next.Parent != initial.ID {
		t.Fatal("a cosmetic change created an intermediate version")
	}
	entry, err := f.s.entryInSnapshot(next.Root, "folder/.DS_Store")
	if err != nil {
		t.Fatal("real change omitted Finder file", err)
	}
	if err := objectfmt.VerifyFile(entry.Digest, f.s.load); err != nil {
		t.Fatal("Finder file content unavailable", err)
	}
	if err := os.Remove(finder); err != nil {
		t.Fatal(err)
	}
	capture(false)
	manual, err := f.s.Snapshot(ctx, f.unit, "explicit checkpoint")
	if err != nil || manual.ID == next.ID || manual.Parent == nil || *manual.Parent != next.ID {
		t.Fatal("explicit checkpoint", manual, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(file, 0700); err != nil {
			t.Fatal(err)
		}
		capture(true)
		link := filepath.Join(dir, "link")
		if err := os.Symlink("data", link); err != nil {
			t.Fatal(err)
		}
		capture(true)
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("elsewhere", link); err != nil {
			t.Fatal(err)
		}
		capture(true)
	}
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	capture(true)
}
