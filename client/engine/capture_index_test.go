package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestIncrementalCaptureTracksRewritesAndMissingObjects(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if root, err := os.Open(f.source); err != nil {
		t.Fatal(err)
	} else {
		_, ok := captureVolume(root)
		root.Close()
		if !ok {
			t.Skip("volume does not provide trustworthy file change times")
		}
	}
	file := filepath.Join(f.source, "changed")
	stable := filepath.Join(f.source, "stable")
	if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	body := []byte("stable content")
	if err := os.WriteFile(stable, body, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	// Wait across the conservative filesystem timestamp resolution boundary.
	time.Sleep(2100 * time.Millisecond)
	v1, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = f.s.db.QueryRow("SELECT count(*) FROM capture_file_index WHERE unit=?", f.unit).Scan(&n); err != nil || n != 2 {
		t.Fatalf("cached %d files: %v", n, err)
	}
	if unchanged, err := f.s.Snapshot(ctx, f.unit, ""); err != nil || unchanged.ID != v1.ID {
		t.Fatalf("unchanged capture %+v: %v", unchanged, err)
	}
	if err = os.WriteFile(file, []byte("edited"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(file, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	// Simulate spool cleanup between scans. Reusing the other file must still
	// produce a complete snapshot even if its cached chunk is gone.
	chunk := f.s.objectPath(objectfmt.Ref{Kind: "chunk", Digest: objectfmt.Digest(body)})
	if err = os.Remove(chunk); err != nil {
		t.Fatal(err)
	}
	v2, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil || v2.ID == v1.ID {
		t.Fatalf("same-length, same-mtime edit was lost: %+v %v", v2, err)
	}
	if repaired, err := os.ReadFile(chunk); err != nil || !bytes.Equal(repaired, body) {
		t.Fatalf("spool repair failed: %v", err)
	}
	if _, err = f.s.AutoSync(ctx, v1, AutoPolicy{AwaySmall: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.AutoSync(ctx, v2, AutoPolicy{AwaySmall: true}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"changed": "edited", "stable": string(body)} {
		out, err := f.s.Cache(ctx, "fetch", f.unit, v2.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		read, err := os.ReadFile(out.Local)
		if err != nil || string(read) != want {
			t.Fatalf("restored %s: %q %v", name, read, err)
		}
	}
	if err = os.Remove(stable); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Snapshot(ctx, f.unit, ""); err != nil {
		t.Fatal(err)
	}
	if err = f.s.db.QueryRow("SELECT count(*) FROM capture_file_index WHERE unit=? AND path='stable'", f.unit).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stale index row after deletion: %v", err)
	}
}

func TestDirectoryCheckDetectsNestedConcurrentEdit(t *testing.T) {
	rootPath := t.TempDir()
	dir := filepath.Join(rootPath, "a", "b")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	c := capture{ctx: t.Context(), root: root, timeless: true}
	if _, err := c.entry("."); err != nil {
		t.Fatal(err)
	}
	if err := c.verifyChecks(); err != nil {
		t.Fatalf("unchanged tree: %v", err)
	}
	if err := os.WriteFile(path, []byte("during capture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.verifyChecks(); err == nil {
		t.Fatal("a concurrent nested file change was missed")
	}
}
