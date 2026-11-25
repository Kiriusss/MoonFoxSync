package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeReadBoundaries(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	prefix := strings.Repeat("a", (128<<10)-1)
	for name, data := range map[string][]byte{"cut": []byte(prefix + "狐尾"), "binary": {1, 0, 2}, "invalid": {0x80, 0xff}, "empty": {}} {
		if e := os.WriteFile(filepath.Join(f.source, name), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.Mkdir(filepath.Join(f.source, "dir"), 0700); e != nil {
		t.Fatal(e)
	}
	v, e := f.s.Snapshot(ctx, f.unit, "test")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.Sync(ctx, f.unit, v.ID); e != nil {
		t.Fatal(e)
	}
	r, e := f.s.Remote(ctx)
	if e != nil {
		t.Fatal(e)
	}
	for name, kind := range map[string]string{"cut": "text", "binary": "binary", "invalid": "binary", "empty": "text", "missing": "missing", "dir": "directory"} {
		p, e := r.MergeRead(ctx, f.unit, v.ID, name)
		if e != nil || p.Kind != kind {
			t.Fatal(name, p.Kind, e)
		}
		if name == "cut" && (!p.Truncated || !bytes.Equal([]byte(p.Text), []byte(prefix))) {
			t.Fatal("partial UTF-8 sequence was not trimmed at preview boundary")
		}
	}
	_, e = r.MergeRead(ctx, f.unit, newID(), "missing")
	var api *APIError
	if !errors.As(e, &api) || api.HTTP != 404 {
		t.Fatal("expected missing snapshot", e)
	}
}
