package engine

import (
	"context"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
	"kirius.local/kiriusdrive/server"
)

func TestSharedProjectAcrossDevices(t *testing.T) {
	keyring.MockInit()
	t.Setenv("KDRIVE_TOKEN_FILE", "")
	ctx := context.Background()
	dir := t.TempDir()
	if e := server.Init(dir); e != nil {
		t.Fatal(e)
	}
	nas, e := server.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer nas.Close()
	web := httptest.NewTLSServer(nas.Handler())
	defer web.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: web.Certificate().Raw})
	scope, e := nas.AddUnit("team")
	if e != nil {
		t.Fatal(e)
	}
	separate, e := nas.AddUnit("unrelated")
	if e != nil {
		t.Fatal(e)
	}
	type device struct {
		s  *Store
		r  *Remote
		id string
	}
	pair := func(scope, role string) device {
		s, e := Open(t.TempDir())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { s.Close() })
		code, e := nas.Issue(scope, role)
		if e != nil {
			t.Fatal(e)
		}
		resp, e := s.Pair(ctx, web.URL, "computer", code, ca)
		if e != nil {
			t.Fatal(e)
		}
		r, e := s.Remote(ctx)
		if e != nil {
			t.Fatal(e)
		}
		return device{s, r, resp["deviceId"].(string)}
	}
	a, b, reader, other := pair(scope, "sync"), pair(scope, "sync"), pair(scope, "reader"), pair(separate, "sync")
	const key = "git:demo"
	var wg sync.WaitGroup
	var result [2]ProjectInfo
	var errs [2]error
	for i, d := range []device{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); result[i], errs[i] = d.r.EnsureProject(ctx, scope, key, "源码") }()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || result[0].UnitID != result[1].UnitID || result[0].Created == result[1].Created {
		t.Fatalf("register: %#v %v", result, errs)
	}
	unit := result[0].UnitID
	if p, e := b.r.Projects(ctx, scope); e != nil || len(p.Items) != 1 {
		t.Fatal(p, e)
	}
	if _, e = reader.r.EnsureProject(ctx, scope, key, "源码"); e == nil {
		t.Fatal("read-only registered")
	}
	if _, e = other.r.Projects(ctx, scope); e == nil {
		t.Fatal("foreign namespace enumerated")
	}
	if _, e = b.r.EnsureProject(ctx, unit, "nested", "nested"); e == nil {
		t.Fatal("nested project namespace accepted")
	}
	p, e := other.r.EnsureProject(ctx, separate, key, "源码")
	if e != nil || p.UnitID == unit {
		t.Fatal("scope isolation", p, e)
	}
	src := t.TempDir()
	doc := filepath.Join(src, "main.txt")
	write := func(p, v string) {
		if e := os.WriteFile(p, []byte(v), 0600); e != nil {
			t.Fatal(e)
		}
	}
	contains := func(p, v string) {
		t.Helper()
		b, e := os.ReadFile(p)
		if e != nil || string(b) != v {
			t.Fatalf("%s: %q %v", p, b, e)
		}
	}
	publish := func(s *Store, label, outcome string) Snapshot {
		t.Helper()
		snap, e := s.Snapshot(ctx, unit, label)
		if e != nil {
			t.Fatal(e)
		}
		ack, e := s.Sync(ctx, unit, snap.ID)
		if e != nil || ack.Outcome != outcome {
			t.Fatalf("publish %v %v", ack, e)
		}
		return snap
	}
	write(doc, "a1")
	write(filepath.Join(src, "obsolete"), "old")
	if _, e = a.s.Bind(ctx, unit, src, "project", false, ""); e != nil {
		t.Fatal(e)
	}
	first := publish(a.s, "first", "published")
	work := filepath.Join(t.TempDir(), "checkout")
	next, e := b.s.CheckoutProject(ctx, unit, work)
	if e != nil || next.Base == nil || *next.Base != first.ID {
		t.Fatal(next, e)
	}
	contains(filepath.Join(work, "main.txt"), "a1")
	if snap, e := b.s.Snapshot(ctx, unit, ""); e != nil || snap.ID != first.ID {
		t.Fatalf("unmodified checkout generated extra snapshot: %+v, %v", snap, e)
	}
	if _, e = b.s.CheckoutProject(ctx, unit, work); e != nil {
		t.Fatal("retry successful checkout", e)
	}
	if _, e = other.s.CheckoutProject(ctx, unit, filepath.Join(t.TempDir(), "forbidden")); e == nil {
		t.Fatal("unauthorized checkout")
	}
	write(doc, "a2")
	if e = os.Remove(filepath.Join(src, "obsolete")); e != nil {
		t.Fatal(e)
	}
	second := publish(a.s, "remote", "published")
	write(filepath.Join(work, "main.txt"), "b2")
	local, e := b.s.Snapshot(ctx, unit, "concurrent local")
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "new-main")
	if _, e = b.s.CheckoutProject(ctx, unit, dest); e == nil {
		t.Fatal("switched with queued local work")
	}
	if _, e = os.Lstat(dest); !os.IsNotExist(e) {
		t.Fatal("created failed checkout destination")
	}
	receipt, e := b.s.Sync(ctx, unit, local.ID)
	if e != nil || receipt.Outcome != "fork" {
		t.Fatalf("fork: %v %v", receipt, e)
	}
	write(filepath.Join(work, "extra-untracked"), "unsaved local")
	oldBinding, err := b.s.binding(ctx, unit)
	if err != nil {
		t.Fatal(err)
	}
	next, e = b.s.CheckoutProject(ctx, unit, dest)
	if e != nil || *next.Base != second.ID {
		t.Fatal(next, e)
	}
	contains(filepath.Join(dest, "main.txt"), "a2")
	if _, e = os.Stat(filepath.Join(dest, "obsolete")); !os.IsNotExist(e) {
		t.Fatal("remote deletion not represented")
	}
	contains(filepath.Join(work, "main.txt"), "b2")
	contains(filepath.Join(work, "obsolete"), "old")
	contains(filepath.Join(work, "extra-untracked"), "unsaved local")
	// Replay the local state a power loss between publishing the directory
	// and committing the binding would leave.
	oldPath, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	oldMarker, err := directoryMarkerAt(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.s.db.Exec("UPDATE bindings SET path=?,base=?,generation=? WHERE unit=?", oldPath, local.ID, oldBinding.generation, unit); err != nil {
		t.Fatal(err)
	}
	if _, err = b.s.db.Exec("UPDATE binding_roots SET identity=? WHERE unit=?", oldMarker, unit); err != nil {
		t.Fatal(err)
	}
	if _, err = b.s.CheckoutProject(ctx, unit, dest); err != nil {
		t.Fatal("finish interrupted checkout", err)
	}
	collision := t.TempDir()
	write(filepath.Join(collision, "mine"), "private")
	if _, err = b.s.CheckoutProject(ctx, unit, collision); err == nil {
		t.Fatal("overwrote unrelated directory")
	}
	contains(filepath.Join(collision, "mine"), "private")
	write(filepath.Join(dest, "main.txt"), "from B")
	third := publish(b.s, "B new head", "published")
	returned := filepath.Join(t.TempDir(), "A-new-main")
	next, e = a.s.CheckoutProject(ctx, unit, returned)
	if e != nil || *next.Base != third.ID {
		t.Fatal(next, e)
	}
	contains(filepath.Join(returned, "main.txt"), "from B")
	if e = nas.Grant(scope, b.id, "reader"); e != nil {
		t.Fatal(e)
	}
	if _, e = b.r.History(ctx, unit); e == nil {
		t.Fatal("downgraded membership retained project access")
	}
	if _, e = b.r.Projects(ctx, scope); e == nil {
		t.Fatal("downgraded membership retained enumeration")
	}
	if e = nas.Grant(unit, reader.id, "reader"); e != nil {
		t.Fatal(e)
	}
	if _, e = reader.r.History(ctx, unit); e != nil {
		t.Fatal("explicit child reader", e)
	}
	if _, e = reader.s.CheckoutProject(ctx, unit, filepath.Join(t.TempDir(), "denied")); e == nil {
		t.Fatal("reader acquired a write binding")
	}
}
