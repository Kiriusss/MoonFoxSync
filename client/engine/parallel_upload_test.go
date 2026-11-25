package engine

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type parallelBatchProbe struct {
	http.RoundTripper
	release chan struct{}
}

func (p parallelBatchProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/batch") {
		req.Body = &gatedBody{ReadCloser: req.Body, release: p.release}
	}
	return p.RoundTripper.RoundTrip(req)
}

func TestParallelSmallFileLimitAndProjectProgress(t *testing.T) {
	for _, limit := range []int{1, 4, 50} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := setup(t)
			ctx := t.Context()
			if _, err := f.s.SetTransferSettings(ctx, &limit, nil); err != nil {
				t.Fatal(err)
			}
			const nbytes = 128 << 10
			for i := 0; i < limit+1; i++ {
				if err := os.WriteFile(filepath.Join(f.source, fmt.Sprintf("%03d.bin", i)), bytes.Repeat([]byte{byte(i)}, nbytes), 0600); err != nil {
					t.Fatal(err)
				}
			}
			v := pendingSnapshot(t, f.s, f.unit, "snapshot")
			r, err := f.s.Remote(ctx)
			if err != nil {
				t.Fatal(err)
			}
			gate := make(chan struct{})
			r.client.Transport = parallelBatchProbe{r.client.Transport, gate}
			done := make(chan error, 1)
			go func() { _, err := f.s.syncUsing(ctx, r, f.unit, v.ID); done <- err }()
			wantTotal := int64(nbytes * (limit + 1))
			observed := false
			deadline := time.Now().Add(10 * time.Second)
			var count int
			for time.Now().Before(deadline) {
				rows, err := ActiveTransfers(f.s.Home)
				if err != nil {
					close(gate)
					<-done
					t.Fatal(err)
				}
				count = len(rows)
				if len(rows) == limit && rows[0].Path != "" && rows[0].ProjectProgress != nil {
					paths := map[string]bool{}
					for _, row := range rows {
						paths[row.Path] = true
						if row.OperationID != rows[0].OperationID || row.Total != wantTotal || row.FilesTotal != int64(limit+1) || row.Bytes != rows[0].Bytes {
							close(gate)
							<-done
							t.Fatalf("wrong project count: %+v", row)
						}
					}
					if len(paths) == limit && rows[0].Bytes > 0 && rows[0].Bytes < wantTotal {
						observed = true
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			close(gate)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !observed {
				t.Fatalf("concurrent file paths not seen, last row count %d", count)
			}
		})
	}
}

func TestProjectProgressDuplicateTreesAndReusedParent(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(12345, 0)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(f.source, "base", "old"), "old")
	write(filepath.Join(f.source, "empty"), "")
	base := pendingSnapshot(t, f.s, f.unit, "base")
	if _, err := f.s.Sync(ctx, f.unit, base.ID); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"copy-a", "copy-b"} {
		write(filepath.Join(f.source, dir, "file"), "abcdef")
	}
	if err := os.Symlink("copy-a", filepath.Join(f.source, "link")); err != nil {
		t.Skip(err)
	}
	v := pendingSnapshot(t, f.s, f.unit, "next")
	progress := &transfer{}
	tr, err := f.s.uploadTracker(ctx, progress, v, false)
	if err != nil {
		t.Fatal(err)
	}
	assert := func(done, files int64) {
		t.Helper()
		if p := progress.project; p.Total != 15 || p.FilesTotal != 4 || p.Bytes != done || p.FilesDone != files {
			t.Fatalf("wanted %d bytes/%d files, got %+v", done, files, p)
		}
	}
	assert(3, 2)
	if len(tr.files) != 1 || len(tr.files[0]) != 1 {
		t.Fatal("duplicate chunk was scheduled twice")
	}
	o := tr.files[0][0]
	id := objectID{o.Kind, o.Digest}
	watch := tr.rawObserver(id, o.Size)
	watch(0, 3)
	assert(6, 2)
	watch(0, 3)
	assert(6, 2)
	tr.confirm(id)
	assert(15, 4)
}

func TestTransferSettingsPersistAndUpdateSeparately(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	got, err := f.s.TransferSettings(ctx)
	if err != nil || got != (TransferSettings{4, 4}) {
		t.Fatal(got, err)
	}
	up, down := 50, 1
	if _, err := f.s.SetTransferSettings(ctx, &up, nil); err != nil {
		t.Fatal(err)
	}
	s, err := Open(f.s.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err = s.SetTransferSettings(ctx, nil, &down); err != nil || got != (TransferSettings{50, 1}) {
		t.Fatal(got, err)
	}
	for _, bad := range []int{0, 51} {
		if _, err := s.SetTransferSettings(ctx, &bad, &up); err == nil {
			t.Fatal("invalid value accepted")
		}
	}
	if got, err = f.s.TransferSettings(ctx); err != nil || got != (TransferSettings{50, 1}) {
		t.Fatal(got, err)
	}
}
