package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stop incoming requests after they have acquired the real cross-process slot.
type restoreGate struct {
	transport http.RoundTripper
	arrivals  chan string
	release   <-chan struct{}
}

func (g restoreGate) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/content") {
		g.arrivals <- req.URL.Query().Get("path")
		select {
		case <-g.release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return g.transport.RoundTrip(req)
}

func TestParallelRestoreSharedSlotsAndCancellation(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	src := filepath.Join(f.source, "folder")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two", "three", "four"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte("body-"+name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2025, 2, 5, 10, 11, 12, 0, time.UTC)
	if err := os.Chtimes(src, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	snap, err := f.s.Snapshot(ctx, f.unit, "download fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(ctx, f.unit, snap.ID); err != nil {
		t.Fatal(err)
	}
	limit := 2
	if _, err = f.s.SetTransferSettings(ctx, nil, &limit); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	arrivals := make(chan string, 20)
	release := make(chan struct{})
	r.client.Transport = restoreGate{r.client.Transport, arrivals, release}
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	// Occupy a slot as a separate foreground command would.
	free, err := acquireFileSlot(ctx, f.s.Home, "download", limit)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restore")
	go func() { _, e := r.Restore(running, f.unit, snap.ID, "", destination); finished <- e }()
	next := func() string {
		select {
		case name := <-arrivals:
			return name
		case <-time.After(3 * time.Second):
			t.Fatal("download worker did not start")
			return ""
		}
	}
	first := next()
	select {
	case <-arrivals:
		t.Fatal("cross-process download limit was exceeded")
	case <-time.After(90 * time.Millisecond):
	}
	free()
	second := next()
	if first == second {
		t.Fatal("duplicate file worker")
	}
	deadline := time.Now().Add(3 * time.Second)
	var observed bool
	for time.Now().Before(deadline) {
		active, e := ActiveTransfers(f.s.Home)
		if e != nil {
			t.Fatal(e)
		}
		if len(active) == 2 && active[0].Path != active[1].Path {
			observed = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !observed {
		t.Fatal("per-file transfer progress not visible")
	}
	cancel()
	if err = <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("published canceled restore")
	}
	if items, err := os.ReadDir(filepath.Dir(destination)); err != nil || len(items) != 0 {
		t.Fatal("staging leaked", items, err)
	}
	close(release)
	report, err := r.Restore(ctx, f.unit, snap.ID, "", destination)
	if err != nil || report.Files != 4 {
		t.Fatal(report, err)
	}
	for _, name := range []string{"one", "two", "three", "four"} {
		b, e := os.ReadFile(filepath.Join(destination, "folder", name))
		if e != nil || !bytes.Equal(b, []byte("body-"+name)) {
			t.Fatal(name, e)
		}
	}
	st, err := os.Stat(filepath.Join(destination, "folder"))
	if err != nil || !st.ModTime().Equal(stamp) {
		t.Fatal("lost parent directory timestamp", st, err)
	}
}

func TestRestoreRejectsIndirectlyEscapingLink(t *testing.T) {
	f := setup(t)
	// Lexical normalization of "alias/../escape" appears inside the tree,
	// but alias points to the root before the .. is resolved.
	if err := os.Symlink(".", filepath.Join(f.source, "alias")); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink("alias/../escape", filepath.Join(f.source, "unsafe")); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	snap, err := f.s.Snapshot(ctx, f.unit, "link")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(ctx, f.unit, snap.ID); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restore")
	if _, err = r.Restore(ctx, f.unit, snap.ID, "", dst); err == nil {
		t.Fatal("published an indirect link outside the staging root")
	}
	if _, err = os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
