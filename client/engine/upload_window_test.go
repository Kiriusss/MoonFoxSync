package engine

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Hold raw PUTs before their bodies go on the wire. Losing the first successful
// response models a NAS commit that the sender must discover on the next offer.
type windowProbe struct {
	base         http.RoundTripper
	gate         chan struct{}
	started      chan struct{}
	mu           sync.Mutex
	active, peak int
	lose         bool
	lost         string
	calls        map[string]int
}

func (p *windowProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != "PUT" {
		return p.base.RoundTrip(req)
	}
	path := req.URL.Path
	p.mu.Lock()
	p.calls[path]++
	p.active++
	p.peak = max(p.peak, p.active)
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.active--; p.mu.Unlock() }()
	p.started <- struct{}{}
	select {
	case <-p.gate:
	case <-req.Context().Done():
		req.Body.Close()
		return nil, req.Context().Err()
	}
	resp, err := p.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	drop := p.lose && resp.StatusCode == 204
	if drop {
		p.lost = path
		p.lose = false
	}
	p.mu.Unlock()
	if drop {
		resp.Body.Close()
		return nil, errors.New("test: lost object acknowledgement")
	}
	return resp, nil
}

func TestRawUploadWindowAndLostReceipt(t *testing.T) {
	for _, limit := range []int{maxRawUploads, 1} {
		t.Run(fmt.Sprintf("workers-%d", limit), func(t *testing.T) {
			f := setup(t)
			for i := range 6 {
				data := bytes.Repeat([]byte{byte(i)}, 1<<20)
				if err := os.WriteFile(filepath.Join(f.source, fmt.Sprintf("%d.bin", i)), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			snap, err := f.s.Snapshot(ctx, f.unit, "batch")
			if err != nil {
				t.Fatal(err)
			}
			remote, err := f.s.Remote(ctx)
			if err != nil {
				t.Fatal(err)
			}
			probe := &windowProbe{base: remote.client.Transport, gate: make(chan struct{}), started: make(chan struct{}, 24), calls: map[string]int{}, lose: true}
			remote.client.Transport = probe
			if limit == 1 {
				remote.client.Transport = &meter{next: probe, limit: smallLimit}
			}
			done := make(chan error, 1)
			go func() { _, e := f.s.syncUsing(ctx, remote, f.unit, snap.ID); done <- e }()
			timeout := time.After(7 * time.Second)
			for range limit {
				select {
				case <-probe.started:
				case <-timeout:
					close(probe.gate)
					<-done
					t.Fatal("raw upload window did not fill")
				}
			}
			close(probe.gate)
			if <-done == nil {
				t.Fatal("lost response must leave the snapshot queued")
			}
			if _, err := f.s.syncUsing(ctx, remote, f.unit, snap.ID); err != nil {
				t.Fatal(err)
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if probe.peak != limit || probe.lost == "" || probe.calls[probe.lost] != 1 {
				t.Fatalf("window=%d lost=%q calls=%v", probe.peak, probe.lost, probe.calls)
			}
		})
	}
}

func TestConcurrentFileProgress(t *testing.T) {
	f := &uploadFile{path: "large", total: 300}
	first, second := objectID{"chunk", "first"}, objectID{"chunk", "second"}
	u := &uploadTracker{transfer: &transfer{}, rawRead: make(map[objectID]int64), uses: map[string][]chunkUse{first.Digest: {{f, 200}}, second.Digest: {{f, 100}}}}
	a, b := u.rawObserver(first, 100), u.rawObserver(second, 100)
	a(0, 50)
	b(0, 75)
	a(0, 25)
	if u.status.FileBytes != 125 || u.payload != 150 {
		t.Fatalf("replayed range: %d/%d", u.status.FileBytes, u.payload)
	}
	a(50, 50)
	u.confirm(first)
	b(75, 25)
	u.confirm(second)
	if f.done != 300 || f.sending != 0 || u.status.FileBytes != 300 || u.payload != 225 {
		t.Fatalf("final file=%+v status=%+v wire=%d", f, u.status, u.payload)
	}
}
