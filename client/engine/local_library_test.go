package engine

import (
	"context"
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
	lock, err := f.s.localLock(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RemoveLocal(ctx, f.unit, plan.Token); err == nil {
		t.Fatal("raced active snapshot")
	}
	lock.Close()
	if _, err = f.s.RemoveLocal(ctx, f.unit, plan.Token); err != nil {
		t.Fatal(err)
	}
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
