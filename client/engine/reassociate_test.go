package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestReassociateKeepsWorktreeAndBothProjectHistories(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r, e := f.s.Remote(ctx)
	if e != nil {
		t.Fatal(e)
	}
	register := func(key string) string {
		t.Helper()
		p, e := r.EnsureProject(ctx, f.unit, key, key)
		if e != nil {
			t.Fatal(e)
		}
		return p.UnitID
	}
	a, b := register("previous"), register("alternative")
	dir := t.TempDir()
	doc := filepath.Join(dir, "source")
	write := func(s string) {
		t.Helper()
		if e := os.WriteFile(doc, []byte(s), 0600); e != nil {
			t.Fatal(e)
		}
	}
	publish := func(id string, snap Snapshot, outcome string) {
		t.Helper()
		receipt, e := f.s.Sync(ctx, id, snap.ID)
		if e != nil || receipt.Outcome != outcome {
			t.Fatalf("%s %v %v", outcome, receipt, e)
		}
	}
	write("one")
	if _, e = f.s.Bind(ctx, a, dir, "project", false, ""); e != nil {
		t.Fatal(e)
	}
	a1, e := f.s.Snapshot(ctx, a, "old work")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.ReassociateProject(ctx, a, b, dir, ""); e == nil {
		t.Fatal("transferred a queued job")
	}
	publish(a, a1, "published")
	info, _ := os.Stat(doc)
	switched, e := f.s.ReassociateProject(ctx, a, b, dir, "")
	if e != nil || switched.Unit != b {
		t.Fatal(switched, e)
	}
	if _, e = f.s.Snapshot(ctx, a, "stale agent"); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("archived unit captured", e)
	}
	after, _ := os.Stat(doc)
	if !os.SameFile(info, after) || info.ModTime() != after.ModTime() {
		t.Fatal("worktree modified")
	}
	replay, e := f.s.ReassociateProject(ctx, a, b, dir, "")
	if e != nil || replay.generation != switched.generation {
		t.Fatal("replay", replay, e)
	}
	if _, e = f.s.ReassociateProject(ctx, f.unit, b, dir, ""); e == nil {
		t.Fatal("accepted incorrect previous ID")
	}
	old, e := f.s.snap(ctx, a1.ID)
	if e != nil || old.Unit != a || old.State != "published" {
		t.Fatal("local original receipt lost", old, e)
	}
	write("legitimate change")
	b1, e := f.s.Snapshot(ctx, b, "new project")
	if e != nil {
		t.Fatal(e)
	}
	publish(b, b1, "published")
	if _, e = f.s.ReassociateProject(ctx, b, a, dir, ""); e == nil {
		t.Fatal("accepted unconfirmed head")
	}
	returned, e := f.s.ReassociateProject(ctx, b, a, dir, a1.ID)
	if e != nil || !returned.ProtectHead || returned.Unit != a {
		t.Fatal(returned, e)
	}
	divergent, e := f.s.Snapshot(ctx, a, "differs from old project")
	if e != nil {
		t.Fatal(e)
	}
	publish(a, divergent, "fork")
	ah, e := r.History(ctx, a)
	if e != nil || len(ah.Items) != 2 {
		t.Fatal(ah, e)
	}
	bh, e := r.History(ctx, b)
	if e != nil || len(bh.Items) != 1 {
		t.Fatal(bh, e)
	}
	if saved, e := f.s.snap(ctx, b1.ID); e != nil || saved.Unit != b || saved.State != "published" {
		t.Fatal(saved, e)
	}
	active, e := f.s.Bindings(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range active {
		if v.Unit == b {
			t.Fatal("archived project active")
		}
	}
	backup := t.TempDir()
	if _, e = f.s.Bind(ctx, b, backup, "project", false, b1.ID); e != nil {
		t.Fatal("archived target could not be independently rebound", e)
	}
	if _, e = f.s.ReassociateProject(ctx, a, b, dir, b1.ID); e == nil {
		t.Fatal("stole occupied target")
	}
}

func TestReassociateRacesWithCaptureAndOtherSelection(t *testing.T) {
	for _, competingCapture := range []bool{false, true} {
		f := setup(t)
		ctx := context.Background()
		r, e := f.s.Remote(ctx)
		if e != nil {
			t.Fatal(e)
		}
		a, e := r.EnsureProject(ctx, f.unit, "a", "A")
		if e != nil {
			t.Fatal(e)
		}
		b, e := r.EnsureProject(ctx, f.unit, "b", "B")
		if e != nil {
			t.Fatal(e)
		}
		second, e := Open(f.s.Home)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { second.Close() })
		if e = os.WriteFile(filepath.Join(f.source, "value"), []byte("not changed by switching"), 0600); e != nil {
			t.Fatal(e)
		}
		var wg sync.WaitGroup
		gate := make(chan struct{})
		var errs [2]error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-gate
			_, errs[0] = f.s.ReassociateProject(ctx, f.unit, a.UnitID, f.source, "")
		}()
		go func() {
			defer wg.Done()
			<-gate
			if competingCapture {
				_, errs[1] = second.Snapshot(ctx, f.unit, "during switch")
			} else {
				_, errs[1] = second.ReassociateProject(ctx, f.unit, b.UnitID, f.source, "")
			}
		}()
		close(gate)
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("expected one winner %v", errs)
		}
		active, e := f.s.Bindings(ctx)
		if e != nil || len(active) != 1 {
			t.Fatal(active, e)
		}
		rows, e := f.s.Queue(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if competingCapture && errs[1] == nil {
			if active[0].Unit != f.unit || len(rows) != 1 || rows[0].Unit != f.unit {
				t.Fatal(active, rows)
			}
		} else if len(rows) != 0 || active[0].Unit == f.unit {
			t.Fatal(active, rows)
		}
	}
}

func TestUpgradeFormerBindingSchema(t *testing.T) {
	dir := t.TempDir()
	db, e := sql.Open("sqlite", filepath.Join(dir, "client.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	const old = "00000000000000000000000001"
	_, e = db.Exec(`CREATE TABLE bindings (unit TEXT PRIMARY KEY,path TEXT UNIQUE NOT NULL,profile TEXT NOT NULL,base TEXT);
 CREATE TABLE snapshots (id TEXT PRIMARY KEY,unit TEXT NOT NULL REFERENCES bindings(unit),parent TEXT,root TEXT NOT NULL,label TEXT NOT NULL,captured TEXT NOT NULL,request TEXT NOT NULL,state TEXT NOT NULL,receipt BLOB);
 INSERT INTO bindings VALUES(?,'/original/path','project','abc'); INSERT INTO snapshots VALUES('abc',?,NULL,'root','old','time','request','published','{}')`, old, old)
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	store, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	active, e := store.Bindings(context.Background())
	if e != nil || len(active) != 1 || active[0].Unit != old || *active[0].Base != "abc" {
		t.Fatal(active, e)
	}
	if _, e = store.db.Exec("UPDATE bindings SET path=NULL WHERE unit=?", old); e != nil {
		t.Fatal(e)
	}
	if rows, e := store.Queue(context.Background()); e != nil || len(rows) != 1 || rows[0].Unit != old {
		t.Fatal(rows, e)
	}
	var exists bool
	if e = store.db.QueryRow("SELECT EXISTS(SELECT 1 FROM pragma_foreign_key_check)").Scan(&exists); e != nil || exists {
		t.Fatal(e)
	}
	if _, e = store.db.Exec("DELETE FROM bindings WHERE unit=?", old); e == nil {
		t.Fatal("foreign keys disabled following migration")
	}
}
