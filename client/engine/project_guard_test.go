package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedCloneCannotDropAnotherComputersIgnoredWeights(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ra, e := f.s.Remote(ctx)
	if e != nil {
		t.Fatal(e)
	}
	project, e := ra.EnsureProject(ctx, f.unit, "git:regression", "实验项目")
	if e != nil {
		t.Fatal(e)
	}
	id := project.UnitID
	git := func(dir string, args ...string) {
		t.Helper()
		// This fixture compares working-tree bytes across clones, including on Git for Windows.
		cmd := exec.Command("git", append([]string{"-c", "core.autocrlf=false", "-c", "user.name=Fixture", "-c", "user.email=fixture@localhost", "-c", "commit.gpgsign=false", "-C", dir}, args...)...)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git %s: %v", out, e)
		}
	}
	write := func(dir, name, body string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	repo := t.TempDir()
	git(repo, "init", "-q")
	write(repo, ".gitignore", "weights.bin\n")
	write(repo, "code.go", "package main\n")
	git(repo, "add", ".")
	git(repo, "commit", "-qm", "base")
	weight := make([]byte, 11<<20)
	weight[len(weight)-1] = 42
	makeWeight := func(dir string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(dir, "weights.bin"), weight, 0600); e != nil {
			t.Fatal(e)
		}
	}
	makeWeight(repo)
	if _, e = f.s.Bind(ctx, id, repo, "project", false, ""); e != nil {
		t.Fatal(e)
	}
	if e = f.s.SetIgnoredSelection(ctx, id, []string{"weights.bin"}); e != nil {
		t.Fatal(e)
	}
	orig, e := f.s.Snapshot(ctx, id, "base")
	if e != nil {
		t.Fatal(e)
	}
	if rec, e := f.s.Sync(ctx, id, orig.ID); e != nil || rec.Outcome != "published" {
		t.Fatal(rec, e)
	}
	clone := func() (*Store, string) {
		t.Helper()
		dst := filepath.Join(t.TempDir(), "different-name")
		git(repo, "clone", "-q", "--no-local", repo, dst)
		// Independent checkouts don't share source mtimes.
		when := time.Unix(1000, 0)
		for _, p := range []string{"code.go", ".gitignore"} {
			if e := os.Chtimes(filepath.Join(dst, p), when, when); e != nil {
				t.Fatal(e)
			}
		}
		s, e := Open(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { s.Close() })
		var ca []byte
		var url string
		if e = f.s.db.QueryRow("SELECT url,ca FROM account WHERE id=1").Scan(&url, &ca); e != nil {
			t.Fatal(e)
		}
		code, e := f.nas.Issue(f.unit, "sync")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Pair(ctx, url, "other", code, ca); e != nil {
			t.Fatal(e)
		}
		return s, dst
	}
	for _, existsButUnselected := range []bool{false, true} {
		b, dst := clone()
		if existsButUnselected {
			makeWeight(dst)
		}
		bound, e := b.ImportProject(ctx, "", id, dst, orig.ID, &IgnoredPolicy{Mode: "selected"})
		if e != nil || !bound.ProtectHead {
			t.Fatal("missing effective file accepted", bound, e)
		}
		if existsButUnselected {
			// Emulate a pre-guard client, which sends no content proof.
			if _, e = b.db.Exec("DELETE FROM project_proofs WHERE unit=?", id); e != nil {
				t.Fatal(e)
			}
		}
		write(dst, "code.go", "package main // B\n")
		snap, e := b.Snapshot(ctx, id, "")
		if e != nil {
			t.Fatal(e)
		}
		ack, e := b.AutoSync(ctx, snap, AutoPolicy{AwaySmall: true})
		if e != nil || ack.Outcome != "fork" || ack.Head == nil || *ack.Head != orig.ID {
			t.Fatal("automatic publish lost remote weight", ack, e)
		}
		if !existsButUnselected {
			restored := filepath.Join(t.TempDir(), "full")
			if _, e = b.CheckoutProject(ctx, id, restored); e != nil {
				t.Fatal(e)
			}
			if info, e := os.Stat(filepath.Join(restored, "weights.bin")); e != nil || info.Size() != int64(len(weight)) {
				t.Fatal(info, e)
			}
			unchanged, e := b.Snapshot(ctx, id, "")
			if e != nil || unchanged.ID != orig.ID {
				t.Fatal("full checkout from earlier Git source", unchanged, e)
			}
		}
	}
	same, dst := clone()
	makeWeight(dst)
	if _, e = same.Bind(ctx, id, dst, "project", false, orig.ID); e != nil {
		t.Fatal(e)
	}
	if e = same.SetIgnoredSelection(ctx, id, []string{"weights.bin"}); e != nil {
		t.Fatal(e)
	}
	verified, e := same.binding(ctx, id)
	if e != nil || verified.ProtectHead {
		t.Fatal("equivalent different-mtime clone remained protected", verified, e)
	}
	write(dst, "code.go", "package main // intentional edit\n")
	updated, e := same.Snapshot(ctx, id, "compatible clone")
	if e != nil {
		t.Fatal(e)
	}
	ack, e := same.Sync(ctx, id, updated.ID)
	if e != nil || ack.Outcome != "published" {
		t.Fatal("matching baseline forked", ack, e)
	}
}
