package engine

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTimelineMutationsWaitForCommitHeaders(t *testing.T) {
	unit, snapshot, head, request := newID(), newID(), newID(), newID()
	entered := make(chan struct{})
	web := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer timeline-test" {
			http.Error(w, "auth", 401)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/restore") {
			var body struct{ Label string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad body", 400)
				return
			}
			if body.Label == "cancel" {
				close(entered)
				<-r.Context().Done()
				return
			}
		}
		select {
		case <-time.After(65 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/delete"):
			_ = json.NewEncoder(w).Encode(map[string]any{"snapshotId": snapshot, "deleted": true, "rebuiltPoints": []any{}})
		case strings.HasSuffix(r.URL.Path, "/restore"):
			_ = json.NewEncoder(w).Encode(map[string]any{"unitId": unit, "snapshotId": snapshot, "outcome": "published"})
		default:
			_ = json.NewEncoder(w).Encode(HistoryPolicy{"all", 0})
		}
	}))
	defer web.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: web.Certificate().Raw})
	r, err := makeRemote(web.URL, ca, "timeline-test")
	if err != nil {
		t.Fatal(err)
	}
	tr := r.client.Transport.(*commitTransport).normal
	tr.ResponseHeaderTimeout = 15 * time.Millisecond
	ctx := context.Background()
	if _, err = r.HistoryPolicy(ctx, unit); err == nil {
		t.Fatal("ordinary read lost its configured timeout")
	}
	if _, err = r.SetHistoryPolicy(ctx, unit, HistoryPolicy{"all", 0}, head); err != nil {
		t.Fatal(err)
	}
	if _, err = r.DeletePoint(ctx, unit, snapshot, head, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err = r.RestorePoint(ctx, unit, snapshot, head, request, "ok"); err != nil {
		t.Fatal(err)
	}
	if tr.ResponseHeaderTimeout != 15*time.Millisecond {
		t.Fatal("shared transport was modified")
	}
	if _, err = r.PreviewDelete(ctx, unit, snapshot, head); err == nil {
		t.Fatal("preview timeout was disabled")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := r.RestorePoint(ctx, unit, snapshot, head, request, "cancel"); result <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler never started")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request ignored cancellation")
	}
}

func TestOfflineForkFromHiddenParentWithMeteredCommit(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	a, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	write := func(root, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(f.source, "private", "old content only available in the deleted point")
	base, err := f.s.Snapshot(ctx, f.unit, "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(ctx, f.unit, base.ID); err != nil {
		t.Fatal(err)
	}
	write(f.source, "offline", "local changes on the old base")
	pending, err := f.s.Snapshot(ctx, f.unit, "offline")
	if err != nil {
		t.Fatal(err)
	}
	// Device B publishes another tree while A is offline.
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var address string
	var ca []byte
	if err = f.s.db.QueryRow("SELECT url,ca FROM account WHERE id=1").Scan(&address, &ca); err != nil {
		t.Fatal(err)
	}
	code, err := f.nas.Issue(f.unit, "sync")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Pair(ctx, address, "B", code, ca); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(dir, "other", "new main")
	if _, err = b.Bind(ctx, f.unit, dir, "project", false, base.ID); err != nil {
		t.Fatal(err)
	}
	main, err := b.Snapshot(ctx, f.unit, "B")
	if err != nil {
		t.Fatal(err)
	}
	if receipt, e := b.Sync(ctx, f.unit, main.ID); e != nil || receipt.Outcome != "published" {
		t.Fatal(receipt, e)
	}
	plan, err := a.PreviewDelete(ctx, f.unit, base.ID, main.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.DeletePoint(ctx, f.unit, base.ID, main.ID, plan.Token); err != nil {
		t.Fatal(err)
	}
	// Leave enough time for ordinary fsync-backed batch requests on a busy
	// disk, while forcing finalize/merge beyond the normal header timeout.
	const ordinaryTimeout = 800 * time.Millisecond
	var finalizes, merges, tlsChecks atomic.Int32
	next := f.nas.Handler()
	web := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/finalize") || strings.HasSuffix(req.URL.Path, "/merge/commit") {
			if strings.HasSuffix(req.URL.Path, "/finalize") {
				finalizes.Add(1)
			} else {
				merges.Add(1)
			}
			select {
			case <-time.After(ordinaryTimeout + 200*time.Millisecond):
			case <-req.Context().Done():
				return
			}
		}
		next.ServeHTTP(w, req)
	}))
	defer web.Close()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: web.Certificate().Raw})
	r, err := makeRemote(web.URL, cert, a.token)
	if err != nil {
		t.Fatal(err)
	}
	wire := r.client.Transport.(*commitTransport)
	wire.normal.ResponseHeaderTimeout = ordinaryTimeout
	wire.normal.TLSClientConfig.VerifyConnection = func(s tls.ConnectionState) error {
		if len(s.VerifiedChains) == 0 {
			return errors.New("TLS chain not verified")
		}
		tlsChecks.Add(1)
		return nil
	}
	longChecks := 0
	blocked := false
	check := func(ctx context.Context) error {
		if ctx.Value(commitWaitKey{}) == true {
			longChecks++
			if blocked {
				return ErrWaiting
			}
		}
		return nil
	}
	budget := &meter{next: checkedTransport{wire, check}, limit: smallLimit}
	r.client.Transport = budget
	receipt, err := f.s.syncUsing(ctx, r, f.unit, pending.ID)
	if err != nil || receipt.Outcome != "fork" {
		t.Fatal(receipt, err)
	}
	if finalizes.Load() != 2 || longChecks != 2 || budget.used <= int64(len("old content only available in the deleted point")) {
		t.Fatal("expected one reoffer through existing guards", finalizes.Load(), longChecks, budget.used)
	}
	restore := filepath.Join(t.TempDir(), "offline")
	if _, err = a.Restore(ctx, f.unit, pending.ID, "", restore); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(restore, "private")); err != nil || string(got) != "old content only available in the deleted point" {
		t.Fatal(string(got), err)
	}
	req := newID()
	blocked = true
	if _, err = r.MergeCommit(ctx, f.unit, main.ID, pending.ID, req, "merge", nil); !errors.Is(err, ErrWaiting) || merges.Load() != 0 {
		t.Fatal("network check bypassed", err)
	}
	blocked = false
	budget.limit = budget.used
	if _, err = r.MergeCommit(ctx, f.unit, main.ID, pending.ID, req, "merge", nil); !errors.Is(err, ErrWaiting) || merges.Load() != 0 {
		t.Fatal("outbound budget bypassed", err)
	}
	budget.limit = smallLimit
	before := budget.used
	if ack, err := r.MergeCommit(ctx, f.unit, main.ID, pending.ID, req, "merge", nil); err != nil || ack.Outcome != "published" {
		t.Fatal(ack, err)
	}
	if merges.Load() != 1 || budget.used <= before || longChecks != 4 || tlsChecks.Load() < 2 {
		t.Fatal(merges.Load(), budget.used, before, longChecks, tlsChecks.Load())
	}
}
