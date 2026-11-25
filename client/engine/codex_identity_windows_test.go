//go:build windows

package engine

import (
	"context"
	"os"
	"testing"
)

// A long-lived writer can overwrite an index entry without extending the file
// or flushing its last-write timestamp. Its cached manifest cannot be trusted.
func TestCodexWindowsOpenWriterBypassesCache(t *testing.T) {
	f := codexFixture(t)
	path := writeCodexFixture(t, f.source, "session_index.jsonl", "{\"v\":1}\n")
	ctx := context.Background()
	v1, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err = w.WriteAt([]byte("{\"v\":2}\n"), 0); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sample, safe, err := codexSignature(r)
	if err != nil || !safe || !sample.volatile {
		t.Fatalf("writer was treated as cacheable: %+v %v %v", sample, safe, err)
	}
	v2, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil || v1.Root == v2.Root {
		t.Fatal(v1, v2, err)
	}
}
