package engine

import (
	"strings"
	"testing"
)

func TestIndependentProjectsMayShareOrigin(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	r, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	origin := "git:" + strings.Repeat("a", 64)
	first, err := r.NewProject(ctx, f.unit, "0a5243d3-d4c2-4916-99e6-f2a6c7410001", "项目 A", origin)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := r.NewProject(ctx, f.unit, "0a5243d3-d4c2-4916-99e6-f2a6c7410001", "项目 A", origin)
	if err != nil || retry.UnitID != first.UnitID || retry.Created {
		t.Fatalf("create retry: %+v, %v", retry, err)
	}
	second, err := r.NewProject(ctx, f.unit, "0a5243d3-d4c2-4916-99e6-f2a6c7410002", "项目 B", origin)
	if err != nil || second.UnitID == first.UnitID || !second.Created {
		t.Fatalf("independent project: %+v, %v", second, err)
	}
	target, err := r.ExistingProject(ctx, f.unit, second.UnitID)
	if err != nil || target.UnitID != second.UnitID || target.SourceKey != origin {
		t.Fatalf("manual selection: %+v, %v", target, err)
	}
	other, err := f.nas.AddUnit("另一个空间")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ExistingProject(ctx, other, second.UnitID); err == nil {
		t.Fatal("selected an unauthorized project")
	}
}
