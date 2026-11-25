package engine

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

// Pause a real HTTP payload at a reproducible byte boundary. The test reads
// the heartbeat through the same file-based API as a separate CLI process.
type gatedBody struct {
	io.ReadCloser
	read    int
	release <-chan struct{}
}

func (g *gatedBody) Read(p []byte) (int, error) {
	const limit = 64 << 10
	if g.read >= limit {
		<-g.release
	} else if len(p) > limit-g.read {
		p = p[:limit-g.read]
	}
	n, e := g.ReadCloser.Read(p)
	g.read += n
	return n, e
}

type gateTransport struct {
	next     http.RoundTripper
	release  <-chan struct{}
	match    string
	download bool
}

func (g gateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target := strings.Contains(req.URL.Path, g.match)
	if target && !g.download {
		req.Body = &gatedBody{ReadCloser: req.Body, release: g.release}
	}
	res, err := g.next.RoundTrip(req)
	if err == nil && target && g.download {
		res.Body = &gatedBody{ReadCloser: res.Body, release: g.release}
	}
	return res, err
}

func TestUploadAndRestoreTransferObservation(t *testing.T) {
	for _, test := range []struct {
		name, endpoint string
		extra          int
	}{
		{"batched", "/batch", 280 << 10}, {"raw", "/objects/", 2 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := setup(t)
			ctx := t.Context()
			const member = "真实文件.bin"
			name := filepath.Join(f.source, member)
			// The unchanged first block in the parent's receipt counts towards
			// the file's logical progress, but never towards this request's rate.
			prefix := bytes.Repeat([]byte{'a'}, objectfmt.ChunkSize)
			if err := os.WriteFile(name, prefix, 0600); err != nil {
				t.Fatal(err)
			}
			base, err := f.s.Snapshot(ctx, f.unit, "initial")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.Sync(ctx, f.unit, base.ID); err != nil {
				t.Fatal(err)
			}
			data := append(prefix, bytes.Repeat([]byte{'b'}, test.extra)...)
			if err = os.WriteFile(name, data, 0600); err != nil {
				t.Fatal(err)
			}
			v, err := f.s.Snapshot(ctx, f.unit, "next")
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(name, name+".changed-after-snapshot"); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "restored")
			for _, direction := range []string{"upload", "download"} {
				r, err := f.s.Remote(ctx)
				if err != nil {
					t.Fatal(err)
				}
				gate := make(chan struct{})
				endpoint := test.endpoint
				if direction == "download" {
					endpoint = "/content"
				}
				r.client.Transport = gateTransport{r.client.Transport, gate, endpoint, direction == "download"}
				done := make(chan error, 1)
				go func() {
					var err error
					if direction == "upload" {
						_, err = f.s.syncUsing(ctx, r, f.unit, v.ID)
					} else {
						_, err = r.Restore(ctx, f.unit, v.ID, "", destination)
					}
					done <- err
				}()
				var observed bool
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					items, e := ActiveTransfers(f.s.Home)
					if e != nil {
						close(gate)
						t.Fatal(e)
					}
					if len(items) == 1 {
						x := items[0]
						minimum := int64(0)
						if direction == "upload" {
							minimum = objectfmt.ChunkSize
						}
						if x.Direction == direction && x.Snapshot == v.ID && x.Unit == f.unit && x.Path == member && x.FileBytes > minimum && x.FileBytes < int64(len(data)) && x.FileTotal == int64(len(data)) && x.BytesPerSecond > 0 && x.BytesPerSecond < 1<<20 {
							observed = true
							break
						}
					}
					time.Sleep(20 * time.Millisecond)
				}
				close(gate)
				if err = <-done; err != nil {
					t.Fatal(err)
				}
				if !observed {
					t.Fatalf("no in-flight %s file/byte progress", direction)
				}
				if items, e := ActiveTransfers(f.s.Home); e != nil || len(items) != 0 {
					t.Fatalf("completed transfer still visible: %v %v", items, e)
				}
			}
			actual, err := os.ReadFile(filepath.Join(destination, member))
			if err != nil || !bytes.Equal(actual, data) {
				t.Fatal("restored content mismatch", err)
			}
		})
	}
}

func TestStaleTransferAndOverlappingOperations(t *testing.T) {
	home := t.TempDir()
	a := startTransfer(home, "upload", "first", "a")
	b := startTransfer(home, "download", "second", "b")
	defer b.close()
	if items, e := ActiveTransfers(home); e != nil || len(items) != 2 {
		t.Fatal(items, e)
	}
	a.close()
	p := filepath.Join(home, "transfers", newID()+".json")
	if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-progressExpiry - time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if items, e := ActiveTransfers(home); e != nil || len(items) != 1 || items[0].ID != b.status.ID {
		t.Fatal(items, e)
	}
	if _, e := os.Stat(p); !os.IsNotExist(e) {
		t.Fatal("expired heartbeat was not removed", e)
	}
}
