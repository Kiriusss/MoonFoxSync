package engine

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func servePending(t *testing.T, f fixture, handler http.Handler) {
	t.Helper()
	web := httptest.NewTLSServer(handler)
	t.Cleanup(web.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: web.Certificate().Raw})
	if _, err := f.s.db.Exec("UPDATE account SET url=?,ca=? WHERE id=1", web.URL, ca); err != nil {
		t.Fatal(err)
	}
}

func pendingSnapshot(t *testing.T, s *Store, unit, label string) Snapshot {
	t.Helper()
	v, err := s.Snapshot(t.Context(), unit, label)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPendingDependencyFailureAndRetry(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	r, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, err := r.NewProject(ctx, f.unit, "0a5243d3-d4c2-4916-99e6-f2a6c7410003", "另一个项目", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Bind(ctx, other.UnitID, t.TempDir(), "project", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetSchedule(ctx, f.unit, "manual", 60); err != nil {
		t.Fatal(err)
	}
	first := pendingSnapshot(t, f.s, f.unit, "parent")
	child := pendingSnapshot(t, f.s, f.unit, "child")
	pendingSnapshot(t, f.s, other.UnitID, "independent")
	// A clock change can put the child before its parent in queue display order.
	if _, err := f.s.db.Exec("UPDATE snapshots SET captured='2020-01-01T00:00:00Z' WHERE id=?", child.ID); err != nil {
		t.Fatal(err)
	}
	next := f.nas.Handler()
	var fail atomic.Bool
	fail.Store(true)
	servePending(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/units/"+f.unit+"/uploads" && fail.Swap(false) {
			http.Error(w, "unavailable", 503)
			return
		}
		next.ServeHTTP(w, r)
	}))
	res, err := f.s.SyncPending(ctx, AutoPolicy{})
	if err != nil || res.Total != 3 || res.Failed != 1 || res.Uploaded != 1 || res.Skipped != 1 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], first.ID) {
		t.Fatalf("partial run: %+v, %v", res, err)
	}
	res, err = f.s.SyncPending(ctx, AutoPolicy{})
	if err != nil || res.Uploaded != 2 || res.Total != 2 || res.Failed != 0 {
		t.Fatalf("retry: %+v, %v", res, err)
	}
	history, err := r.History(ctx, f.unit)
	if err != nil || len(history.Items) != 2 {
		t.Fatalf("history: %+v, %v", history, err)
	}
	cfg, err := f.s.Schedule(ctx, f.unit)
	if err != nil || cfg.Policy != "manual" || cfg.Minutes != 60 {
		t.Fatalf("schedule changed: %+v, %v", cfg, err)
	}
}

func TestPendingInFlight(t *testing.T) {
	// A separate agent process sees the filesystem lock before it opens the
	// network or reserves traffic, so it does not even need access to a keyring.
	if home := os.Getenv("KDRIVE_TEST_BUSY_HOME"); home != "" {
		s, err := Open(home)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		v, err := s.snap(t.Context(), os.Getenv("KDRIVE_TEST_BUSY_SNAPSHOT"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AutoSync(t.Context(), v, AutoPolicy{AwaySmall: true}); !errors.Is(err, ErrWaiting) {
			t.Fatalf("concurrent agent: %v", err)
		}
		return
	}

	f := setup(t)
	ctx := t.Context()
	first := pendingSnapshot(t, f.s, f.unit, "in flight")
	began, release := make(chan struct{}), make(chan struct{})
	var unblock, once sync.Once
	defer unblock.Do(func() { close(release) })
	var starts atomic.Int32
	next := f.nas.Handler()
	servePending(t, f, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/uploads") {
			starts.Add(1)
			once.Do(func() {
				close(began)
				<-release
			})
		}
		next.ServeHTTP(w, r)
	}))
	type outcome struct {
		res PendingSyncResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := f.s.SyncPending(ctx, AutoPolicy{})
		done <- outcome{res, err}
	}()
	select {
	case <-began:
	case <-time.After(10 * time.Second):
		t.Fatal("manual transfer did not start")
	}

	pendingSnapshot(t, f.s, f.unit, "created during transfer")
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPendingInFlight$")
	cmd.Env = append(os.Environ(), "KDRIVE_TEST_BUSY_HOME="+f.s.Home, "KDRIVE_TEST_BUSY_SNAPSHOT="+first.ID)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("concurrent agent: %v %s", err, out)
	}
	duplicate, err := f.s.SyncPending(ctx, AutoPolicy{})
	if err != nil || duplicate.Active != 1 || duplicate.Skipped != 1 || duplicate.Total != 2 || duplicate.Failed != 0 {
		t.Fatalf("repeated click: %+v, %v", duplicate, err)
	}
	if _, err = f.s.Sync(ctx, f.unit, first.ID); !errors.Is(err, errUploadActive) {
		t.Fatalf("single-version upload should also be locked: %v", err)
	}
	unblock.Do(func() { close(release) })
	completed := <-done
	if completed.err != nil || completed.res.Total != 1 || completed.res.Uploaded != 1 || starts.Load() != 1 {
		t.Fatalf("frozen manual queue: %+v, %v, starts=%d", completed.res, completed.err, starts.Load())
	}
	ack, err := f.s.AutoSync(ctx, first, AutoPolicy{AwaySmall: true})
	if err != nil || ack.Snapshot != first.ID || starts.Load() != 1 {
		t.Fatalf("late agent did not reuse receipt: %+v, %v", ack, err)
	}
	var reservations int
	if err = f.s.db.QueryRow("SELECT COUNT(*) FROM traffic").Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("duplicate metered reservations: %d, %v", reservations, err)
	}
	res, err := f.s.SyncPending(ctx, AutoPolicy{})
	if err != nil || res.Total != 1 || res.Uploaded != 1 || starts.Load() != 2 {
		t.Fatalf("next click should upload new child: %+v, %v", res, err)
	}
}
