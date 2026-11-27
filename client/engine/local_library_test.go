package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"
	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestLocalRemovalIsScopedAndResetClearsCredential(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	makeFile(t, f.source, "common", "shared bytes")
	makeFile(t, f.source, "exclusive", "only first project")
	first, err := f.s.Snapshot(ctx, f.unit, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(ctx, f.unit, first.ID); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	another, err := r.EnsureProject(ctx, f.unit, "other", "other")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	makeFile(t, dir, "same", "shared bytes")
	if _, err = f.s.Bind(ctx, another.UnitID, dir, "project", false, ""); err != nil {
		t.Fatal(err)
	}
	otherSnapshot, err := f.s.Snapshot(ctx, another.UnitID, "other")
	if err != nil {
		t.Fatal(err)
	}
	cached, err := f.s.Cache(ctx, "fetch", f.unit, first.ID, "common")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Cache(ctx, "fetch", f.unit, first.ID, "exclusive"); err != nil {
		t.Fatal(err)
	}
	held, err := f.s.Cache(ctx, "evict", f.unit, first.ID, "exclusive")
	if err != nil || held.State != "nas_only" {
		t.Fatal(held, err)
	}
	plan, err := f.s.LocalRemovalPlan(ctx, f.unit)
	if err != nil || plan.Cached != 2 || plan.NASOnly != 1 || plan.Snapshots != 1 || plan.Queued != 0 {
		t.Fatal(plan, err)
	}
	makeFile(t, f.source, "unsent", "pending")
	if _, err = f.s.Snapshot(ctx, f.unit, "pending"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RemoveLocal(ctx, f.unit, plan.Token); err == nil {
		t.Fatal("accepted stale preview")
	}
	plan, err = f.s.LocalRemovalPlan(ctx, f.unit)
	if err != nil || plan.Queued != 1 {
		t.Fatal(plan, err)
	}
	if err = os.Chmod(cached.Local, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cached.Local, []byte("user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RemoveLocal(ctx, f.unit, plan.Token); err == nil {
		t.Fatal("removed edited download")
	}
	if got, err := os.ReadFile(cached.Local); err != nil || string(got) != "user edit" {
		t.Fatal(string(got), err)
	}
	if err = os.WriteFile(cached.Local, []byte("shared bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := f.s.projectActivity(f.unit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RemoveLocal(ctx, f.unit, plan.Token); err == nil {
		t.Fatal("raced active snapshot")
	}
	lock.Close()
	// The capture index outlives individual snapshots and is scoped to a binding.
	for _, unit := range []string{f.unit, another.UnitID} {
		if _, err := f.s.db.Exec("INSERT INTO capture_file_index(unit,parent,path,signature,digest) VALUES(?, '.', 'index-fixture', X'00', '')", unit); err != nil {
			t.Fatal(err)
		}
	}
	checkIndexed := func(unit string, want int) {
		t.Helper()
		var n int
		err := f.s.db.QueryRow("SELECT count(*) FROM capture_file_index WHERE unit=? AND path='index-fixture'", unit).Scan(&n)
		if err != nil || n != want {
			t.Fatalf("index count=%d, want %d: %v", n, want, err)
		}
	}
	if _, err = f.s.RemoveLocal(ctx, f.unit, plan.Token); err != nil {
		t.Fatal(err)
	}
	checkIndexed(f.unit, 0)
	checkIndexed(another.UnitID, 1)
	for _, file := range []string{cached.Local, f.s.objectPath(objectfmt.Ref{Kind: "chunk", Digest: objectfmt.Digest([]byte("only first project"))})} {
		if _, err = os.Stat(file); !os.IsNotExist(err) {
			t.Fatal(file, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(f.source, "unsent")); err != nil || string(got) != "pending" {
		t.Fatal(string(got), err)
	}
	if _, err = f.s.Sync(ctx, another.UnitID, otherSnapshot.ID); err != nil {
		t.Fatal("other project object damaged", err)
	}
	if _, err = r.Restore(ctx, f.unit, first.ID, "", filepath.Join(t.TempDir(), "restored")); err != nil {
		t.Fatal("NAS source affected", err)
	}
	var hold struct {
		State string `json:"state"`
	}
	if err = r.call(ctx, "GET", "/units/"+f.unit+"/holds/"+held.Hold, nil, nil, &hold); err != nil || hold.State != "active" {
		t.Fatal("NAS-only snapshot hold lost", hold, err)
	}
	var device string
	if err = f.s.db.QueryRow("SELECT device FROM account").Scan(&device); err != nil {
		t.Fatal(err)
	}
	reset, err := f.s.LocalRemovalPlan(ctx, "")
	if err != nil || reset.Bindings != 1 {
		t.Fatal(reset, err)
	}
	if _, err = f.s.RemoveLocal(ctx, "", reset.Token); err != nil {
		t.Fatal(err)
	}
	checkIndexed(another.UnitID, 0)
	if _, err = keyring.Get("xinhu-sync", device); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatal(err)
	}
	status, err := f.s.LocalStatus(ctx)
	if err != nil || status.Paired {
		t.Fatal(status, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "same")); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Restore(ctx, another.UnitID, otherSnapshot.ID, "", filepath.Join(t.TempDir(), "preserved")); err != nil {
		t.Fatal("remote changed", err)
	}
}

func TestScopedRemovalWithActiveUploadAndSpoolCapture(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	makeFile(t, f.source, "other.dat", "shared in-flight content")
	first, err := f.s.Snapshot(ctx, f.unit, "target")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := remote.EnsureProject(ctx, f.unit, "sibling", "sibling")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	makeFile(t, dir, "only", "different bytes")
	if _, err = f.s.Bind(ctx, sibling.UnitID, dir, "project", false, ""); err != nil {
		t.Fatal(err)
	}
	other, err := f.s.Snapshot(ctx, sibling.UnitID, "sibling")
	if err != nil {
		t.Fatal(err)
	}
	// The same callback holds the lock throughout a real network upload.
	in := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	}()
	go func() {
		g, e := f.s.localLock(false)
		if e != nil {
			done <- e
			return
		}
		defer g.Close()
		_, _, e = f.s.withUpload(ctx, sibling.UnitID, other.ID, func(Snapshot, string) (*Receipt, error) { close(in); <-release; return nil, nil })
		done <- e
	}()
	<-in
	ro, err := OpenRemovalPreview(f.s.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	writer, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Exec("UPDATE bindings SET base=base WHERE 0"); err != nil {
		t.Fatal(err)
	}
	plan, err := ro.LocalRemovalPlan(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	sibPlan, err := ro.LocalRemovalPlan(ctx, sibling.UnitID)
	if err != nil {
		t.Fatal(err)
	}
	writer.Rollback()
	if _, err = f.s.RemoveLocal(ctx, sibling.UnitID, sibPlan.Token); !errors.Is(err, errProjectInUse) {
		t.Fatal("active target", err)
	}
	reset, err := ro.LocalRemovalPlan(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RemoveLocal(ctx, "", reset.Token); err == nil {
		t.Fatal("reset raced upload")
	}
	// A different capture may already have observed a deduplicated file on
	// disk, but not yet published its references. Leave it until that commit.
	pin, err := f.s.beginSpoolCapture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RemoveLocal(ctx, f.unit, plan.Token); err != nil {
		pin.Close()
		t.Fatal(err)
	}
	chunk := objectfmt.Ref{Kind: "chunk", Digest: objectfmt.Digest([]byte("shared in-flight content"))}
	file := f.s.objectPath(chunk)
	if _, err = os.Stat(file); err != nil {
		t.Fatal("removed candidate during capture", err)
	}
	if _, err = f.s.db.Exec("INSERT INTO snapshot_objects VALUES(?,?,?,?)", other.ID, chunk.Kind, chunk.Digest, len("shared in-flight content")); err != nil {
		t.Fatal(err)
	}
	pin.Close()
	if err = f.s.collectDeferredSpool(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(file); err != nil {
		t.Fatal("newly referenced object lost", err)
	}
	if _, err = f.s.snap(ctx, first.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("target metadata still present", err)
	}
}

func TestSyncPendingSkipsProjectBeingForgotten(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	makeFile(t, f.source, "one", "one")
	if _, err := f.s.Snapshot(ctx, f.unit, "one"); err != nil {
		t.Fatal(err)
	}
	remote, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, err := remote.EnsureProject(ctx, f.unit, "second", "second")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	makeFile(t, dir, "two", "two")
	if _, err = f.s.Bind(ctx, other.UnitID, dir, "project", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Snapshot(ctx, other.UnitID, "two"); err != nil {
		t.Fatal(err)
	}
	exclusive, err := f.s.projectLock(f.unit, true)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	report, err := f.s.SyncPending(ctx, AutoPolicy{})
	if err != nil || report.Failed != 0 || report.Skipped != 1 || report.Uploaded != 1 {
		t.Fatal(report, err)
	}
}
