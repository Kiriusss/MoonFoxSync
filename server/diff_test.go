package server

import (
	"context"
	"testing"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestTreeDiffMovesAndSkipsIdenticalDirectories(t *testing.T) {
	objects := make(map[objectfmt.Ref][]byte)
	store := func(ref objectfmt.Ref, data []byte) error {
		objects[ref] = data
		return nil
	}
	dir := func(entries ...objectfmt.Entry) string {
		t.Helper()
		d, err := objectfmt.BuildDir(entries, store)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	file := func(name, content string) objectfmt.Entry {
		return objectfmt.Entry{Name: name, Kind: "file", Digest: objectfmt.Digest([]byte(content))}
	}
	directory := func(name, digest string) objectfmt.Entry {
		return objectfmt.Entry{Name: name, Kind: "directory", Digest: digest}
	}
	untouched := dir(file("unopened", "x"))
	baseline := dir(file("before", "renamed"), file("duplicate-one", "duplicate"), file("duplicate-two", "duplicate"),
		directory("sub", dir(file("changed", "a"))), directory("same", untouched))
	updated := dir(file("after", "renamed"), file("duplicate-three", "duplicate"),
		directory("sub", dir(file("changed", "b"))), directory("same", untouched))
	load := func(ref objectfmt.Ref) ([]byte, error) {
		if ref.Digest == untouched || ref.Kind != "tree" {
			t.Fatalf("diff opened unchanged tree or file %v", ref)
		}
		return objects[ref], nil
	}
	got, err := compareTrees(context.Background(), baseline, updated, load)
	if err != nil {
		t.Fatal(err)
	}
	want := []change{
		{Path: "after", Status: "moved", OldPath: "before"},
		{Path: "duplicate-one", Status: "deleted"},
		{Path: "duplicate-three", Status: "added"},
		{Path: "duplicate-two", Status: "deleted"},
		{Path: "sub/changed", Status: "modified"},
	}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for i := range got {
		got[i].identity = ""
		if got[i] != want[i] {
			t.Fatalf("difference %d got %+v want %+v", i, got[i], want[i])
		}
	}
}
