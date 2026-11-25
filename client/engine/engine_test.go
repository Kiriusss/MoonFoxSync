package engine

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
	"kirius.local/kiriusdrive/server"
)

type fixture struct {
	s            *Store
	nas          *server.Store
	unit, source string
}

func setup(t *testing.T) fixture {
	t.Helper()
	keyring.MockInit()
	t.Setenv("KDRIVE_TOKEN_FILE", "")
	nasDir := t.TempDir()
	if e := server.Init(nasDir); e != nil {
		t.Fatal(e)
	}
	nas, e := server.Open(nasDir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { nas.Close() })
	web := httptest.NewTLSServer(nas.Handler())
	t.Cleanup(web.Close)
	unit, e := nas.AddUnit("test")
	if e != nil {
		t.Fatal(e)
	}
	code, e := nas.Issue(unit, "sync")
	if e != nil {
		t.Fatal(e)
	}
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: web.Certificate().Raw})
	ctx := context.Background()
	if _, e = s.Pair(ctx, web.URL, "test", code, ca); e != nil {
		t.Fatal(e)
	}
	source := t.TempDir()
	if _, e = s.Bind(ctx, unit, source, "project", false, ""); e != nil {
		t.Fatal(e)
	}
	return fixture{s, nas, unit, source}
}
func TestAutoSmallAndSafeCache(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	original := filepath.Join(f.source, "原稿.txt")
	if e := os.WriteFile(original, []byte("hello"), 0600); e != nil {
		t.Fatal(e)
	}
	v, e := f.s.Snapshot(ctx, f.unit, "v1")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.AutoSync(ctx, v, AutoPolicy{}); !errors.Is(e, ErrWaiting) {
		t.Fatalf("default policy: %v", e)
	}
	if _, e = f.s.AutoSync(ctx, v, AutoPolicy{AwaySmall: true}); e != nil {
		t.Fatal(e)
	}
	cache, e := f.s.Cache(ctx, "fetch", f.unit, v.ID, "原稿.txt")
	if e != nil {
		t.Fatal(e)
	}
	if data, e := os.ReadFile(cache.Local); e != nil || string(data) != "hello" {
		t.Fatal("cache content", e)
	}
	if e = os.Chmod(cache.Local, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(cache.Local, []byte("local changes"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.Cache(ctx, "evict", f.unit, v.ID, "原稿.txt"); e == nil {
		t.Fatal("modified bytes should be preserved")
	}
	if got, e := os.ReadFile(cache.Local); e != nil || string(got) != "local changes" {
		t.Fatal("modified cache lost")
	}
	if e = os.WriteFile(cache.Local, []byte("hello"), 0600); e != nil {
		t.Fatal(e)
	}
	removed, e := f.s.Cache(ctx, "evict", f.unit, v.ID, "原稿.txt")
	if e != nil || removed.State != "nas_only" {
		t.Fatal(removed, e)
	}
	if _, e = os.Stat(cache.Local); !os.IsNotExist(e) {
		t.Fatal("cache not removed", e)
	}
	if got, e := os.ReadFile(original); e != nil || string(got) != "hello" {
		t.Fatal("source lost")
	}
	fetched, e := f.s.Cache(ctx, "fetch", f.unit, v.ID, "原稿.txt")
	if e != nil || fetched.State != "downloaded" {
		t.Fatal(fetched, e)
	}
	if e = os.WriteFile(original, []byte("new contents"), 0600); e != nil {
		t.Fatal(e)
	}
	next, e := f.s.Snapshot(ctx, f.unit, "v2")
	if e != nil || next.Parent == nil || *next.Parent != v.ID {
		t.Fatal(next, e)
	}
	if _, e = f.s.AutoSync(ctx, next, AutoPolicy{AwaySmall: true}); e != nil {
		t.Fatal(e)
	}
	var total int64
	if e = f.s.db.QueryRow("SELECT SUM(charged) FROM traffic").Scan(&total); e != nil || total < 1 || total >= smallLimit {
		t.Fatal(total, e)
	}
	if _, e = f.s.db.Exec("INSERT INTO traffic VALUES(?,?,?,?)", newID(), "other", time.Now().Unix(), dailyLimit-total); e != nil {
		t.Fatal(e)
	}
	v3, e := f.s.Snapshot(ctx, f.unit, "checkpoint")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.AutoSync(ctx, v3, AutoPolicy{AwaySmall: true}); !errors.Is(e, ErrWaiting) {
		t.Fatalf("quota exhausted: %v", e)
	}
}
func TestLargeLogicalFileWaitsOnAway(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := filepath.Join(f.source, "weights.bin")
	file, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = file.Truncate(smallLimit + 1); e != nil {
		t.Fatal(e)
	}
	file.Close()
	v, e := f.s.Snapshot(ctx, f.unit, "large")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.AutoSync(ctx, v, AutoPolicy{AwaySmall: true}); !errors.Is(e, ErrWaiting) {
		t.Fatal(e)
	}
}
func TestRestoreRefusesOccupiedDestination(t *testing.T) {
	r := &Remote{}
	dst := t.TempDir()
	p := filepath.Join(dst, "untouched")
	if e := os.WriteFile(p, []byte("abc"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Restore(context.Background(), newID(), newID(), "", dst); e == nil {
		t.Fatal("overwritten")
	}
	if b, e := os.ReadFile(p); e != nil || string(b) != "abc" {
		t.Fatal(e)
	}
}

func TestReplacedDirectoryCannotAdvanceHistory(t *testing.T) {
	f := setup(t)
	previous := f.source + "-unmounted"
	if e := os.Rename(f.source, previous); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(previous) })
	if e := os.Mkdir(f.source, 0700); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Snapshot(context.Background(), f.unit, ""); e == nil {
		t.Fatal("replacement directory was captured")
	}
	queue, e := f.s.Queue(context.Background())
	if e != nil || len(queue) != 0 {
		t.Fatal(queue, e)
	}
}

func TestGetCurrentExternalFile(t *testing.T) {
	f := setup(t)
	src := t.TempDir()
	if e := os.WriteFile(filepath.Join(src, "sample"), []byte("abc"), 0600); e != nil {
		t.Fatal(e)
	}
	unit, e := f.nas.AddExternal("external", src)
	if e != nil {
		t.Fatal(e)
	}
	var device string
	if e = f.s.db.QueryRow("SELECT device FROM account").Scan(&device); e != nil {
		t.Fatal(e)
	}
	if e = f.nas.Grant(unit, device, "reader"); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	r, e := f.s.Remote(ctx)
	if e != nil {
		t.Fatal(e)
	}
	dst := filepath.Join(t.TempDir(), "saved")
	result, e := r.Get(ctx, unit, "sample", dst)
	if e != nil || result.Bytes != 3 || result.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(result, e)
	}
	if e = os.WriteFile(filepath.Join(src, "sample"), []byte("changed"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Get(ctx, unit, "sample", dst); e == nil {
		t.Fatal("overwrote target")
	}
	if raw, e := os.ReadFile(dst); e != nil || string(raw) != "abc" {
		t.Fatal("download changed", e)
	}
}
