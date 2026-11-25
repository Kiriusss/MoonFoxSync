package engine

import (
	"bytes"
	"context"

	"errors"

	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThreeWaySnapshotMergeOverTLS(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a, e := f.s.Remote(ctx)
	if e != nil {
		t.Fatal(e)
	}
	var ca []byte
	var address string
	if e = f.s.db.QueryRow("SELECT url,ca FROM account WHERE id=1").Scan(&address, &ca); e != nil {
		t.Fatal(e)
	}
	device := func(role string) (*Store, *Remote) {
		t.Helper()
		s, e := Open(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { s.Close() })
		code, e := f.nas.Issue(f.unit, role)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Pair(ctx, address, role, code, ca); e != nil {
			t.Fatal(e)
		}
		r, e := s.Remote(ctx)
		if e != nil {
			t.Fatal(e)
		}
		return s, r
	}
	b, _ := device("sync")
	_, reader := device("reader")
	write := func(root, name, contents string) {
		t.Helper()
		file := filepath.Join(root, name)
		if e := os.MkdirAll(filepath.Dir(file), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(file, []byte(contents), 0600); e != nil {
			t.Fatal(e)
		}
	}
	save := func(s *Store, name, outcome string) Snapshot {
		t.Helper()
		v, e := s.Snapshot(ctx, f.unit, name)
		if e != nil {
			t.Fatal(e)
		}
		ack, e := s.Sync(ctx, f.unit, v.ID)
		if e != nil || ack.Outcome != outcome {
			t.Fatalf("%s %v %v", name, ack, e)
		}
		return v
	}
	for _, n := range []string{"doc.txt", "weights.bin", "drop/file.txt", "flip", "one", "two"} {
		write(f.source, n, "initial")
	}
	root := save(f.s, "base", "published")
	pathB := filepath.Join(t.TempDir(), "B")
	if _, e = b.CheckoutProject(ctx, f.unit, pathB); e != nil {
		t.Fatal(e)
	}
	// Trusted base, unrelated one-sided changes, a delete/modify, file/directory
	// type change, independent additions of subdirectories, binary and text conflicts.
	write(f.source, "doc.txt", "main text")
	write(pathB, "doc.txt", "branch text")
	large := strings.Repeat("z", 2<<20)
	write(f.source, "weights.bin", large+"a")
	write(pathB, "weights.bin", large+"b")
	if e = os.RemoveAll(filepath.Join(f.source, "drop")); e != nil {
		t.Fatal(e)
	}
	write(pathB, "drop/file.txt", "keep subtree")
	write(f.source, "flip", "changed file")
	if e = os.Remove(filepath.Join(pathB, "flip")); e != nil {
		t.Fatal(e)
	}
	write(pathB, "flip/nested", "new directory")
	write(f.source, "one", "main only")
	write(pathB, "two", "branch only")
	write(f.source, "newdir/a", "a")
	write(pathB, "newdir/b", "b")
	if e = os.Chmod(filepath.Join(f.source, "one"), 0700); e != nil {
		t.Fatal(e)
	}
	linked := os.Symlink("two", filepath.Join(pathB, "soft")) == nil
	main := save(f.s, "main", "published")
	fork := save(b, "branch", "fork")
	plan, e := reader.MergePlan(ctx, f.unit, fork.ID)
	if e != nil || plan.MainHead != main.ID || plan.BaseSnapshotID != root.ID || len(plan.Conflicts) != 4 {
		t.Fatal(plan, e)
	}
	var alt string
	for _, c := range plan.Conflicts {
		switch c.Path {
		case "doc.txt":
			if !c.TextAllowed {
				t.Fatal(c)
			}
		case "drop":
			if c.MainKind != "missing" || c.BranchKind != "directory" || c.BothPath != "" {
				t.Fatal(c)
			}
		case "weights.bin":
			if c.TextAllowed || c.BothPath == "" {
				t.Fatal(c)
			}
			alt = c.BothPath
		case "flip":
			if c.BranchKind != "directory" || c.TextAllowed {
				t.Fatal(c)
			}
		default:
			t.Fatal("unexpected conflict", c)
		}
	}
	expect := func(e error, status int) {
		t.Helper()
		var api *APIError
		if !errors.As(e, &api) || api.HTTP != status {
			t.Fatalf("want %d, got %v", status, e)
		}
	}
	req := newID()
	text := "manually merged 中文\n"
	choices := []MergeChoice{{"doc.txt", "text", &text}, {"drop", "branch", nil}, {"flip", "branch", nil}, {"weights.bin", "both", nil}}
	_, e = reader.MergeCommit(ctx, f.unit, main.ID, fork.ID, req, "merge", choices)
	expect(e, 403)
	_, e = a.MergeCommit(ctx, f.unit, main.ID, fork.ID, req, "merge", choices[:2])
	expect(e, 400)
	bad := append([]MergeChoice{}, choices...)
	bad[3] = MergeChoice{"weights.bin", "text", &text}
	_, e = a.MergeCommit(ctx, f.unit, main.ID, fork.ID, req, "merge", bad)
	expect(e, 400)
	// Main advances after review. No stale merge or mutated worktree is accepted.
	write(f.source, "late", "other device change")
	main2 := save(f.s, "late", "published")
	_, e = a.MergeCommit(ctx, f.unit, main.ID, fork.ID, req, "merge", choices)
	expect(e, 409)
	plan, e = a.MergePlan(ctx, f.unit, fork.ID)
	if e != nil || plan.MainHead != main2.ID {
		t.Fatal(plan, e)
	}
	rec, e := a.MergeCommit(ctx, f.unit, main2.ID, fork.ID, req, "merge", choices)
	if e != nil || rec.Outcome != "published" || rec.Snapshot == req {
		t.Fatal(rec, e)
	}
	again, e := a.MergeCommit(ctx, f.unit, main2.ID, fork.ID, req, "merge", choices)
	if e != nil || again.Snapshot != rec.Snapshot {
		t.Fatal(again, e)
	}
	_, e = a.MergeCommit(ctx, f.unit, main2.ID, fork.ID, req, "changed payload", choices)
	expect(e, 409)
	plan, e = a.MergePlan(ctx, f.unit, fork.ID)
	if e != nil || !plan.AlreadyMerged {
		t.Fatal(plan, e)
	}
	var record map[string]any
	if e = a.call(ctx, "GET", "/units/"+f.unit+"/snapshots/"+rec.Snapshot, nil, nil, &record); e != nil || record["parentSnapshotId"] != main2.ID || record["mergeParentSnapshotId"] != fork.ID {
		t.Fatal(record, e)
	}
	dir := filepath.Join(t.TempDir(), "result")
	if _, e = reader.Restore(ctx, f.unit, rec.Snapshot, "", dir); e != nil {
		t.Fatal(e)
	}
	for file, value := range map[string]string{"doc.txt": text, "weights.bin": large + "a", alt: large + "b", "drop/file.txt": "keep subtree", "flip/nested": "new directory", "one": "main only", "two": "branch only", "newdir/a": "a", "newdir/b": "b", "late": "other device change"} {
		got, e := os.ReadFile(filepath.Join(dir, file))
		if e != nil || !bytes.Equal(got, []byte(value)) {
			t.Fatalf("incorrect merged path %s: %v", file, e)
		}
	}
	if linked {
		if got, e := os.Readlink(filepath.Join(dir, "soft")); e != nil || got != "two" {
			t.Fatal(got, e)
		}
	}
	// Working copies are unaffected by NAS publication, including deletes.
	if _, e = os.Stat(filepath.Join(f.source, "drop")); !os.IsNotExist(e) {
		t.Fatal("rewrote main source")
	}
	if got, _ := os.ReadFile(filepath.Join(pathB, "doc.txt")); string(got) != "branch text" {
		t.Fatal("rewrote branch source")
	}
}
