package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewedGitSync(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := filepath.Join(root, "upstream.git")
	fixtureGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	first, second := filepath.Join(root, "a"), filepath.Join(root, "b")
	fixtureGit(t, root, "clone", "-q", origin, first)
	makeFile(t, first, ".gitignore", "ignored.bin\n")
	fixtureGit(t, first, "add", ".gitignore")
	fixtureGit(t, first, "commit", "-qm", "initial")
	fixtureGit(t, first, "push", "-qu", "origin", "main")
	fixtureGit(t, root, "clone", "-q", origin, second)
	fixtureGit(t, first, "commit", "--allow-empty", "-qm", "new")
	plan, err := PreviewGitSync(ctx, first)
	if err != nil || plan.State != "ahead" || plan.Ahead != 1 {
		t.Fatal(plan, err)
	}
	// Another branch with the same commit and the same upstream still needs its own confirmation.
	fixtureGit(t, first, "checkout", "-qb", "substitute")
	fixtureGit(t, first, "branch", "--set-upstream-to=origin/main")
	if _, err = ApplyGitSync(ctx, first, plan.Head, plan.UpstreamHead, plan.PlanToken); err == nil {
		t.Fatal("stale branch confirmation accepted")
	}
	fixtureGit(t, first, "checkout", "-q", "main")
	result, err := ApplyGitSync(ctx, first, plan.Head, plan.UpstreamHead, plan.PlanToken)
	if err != nil || result.State != "equal" {
		t.Fatal(result, err)
	}
	plan, err = PreviewGitSync(ctx, second)
	if err != nil || plan.State != "behind" {
		t.Fatal(plan, err)
	}
	makeFile(t, second, ".gitignore", "work in progress\n")
	if _, err = ApplyGitSync(ctx, second, plan.Head, plan.UpstreamHead, plan.PlanToken); err == nil {
		t.Fatal("dirty pull accepted")
	}
	fixtureGit(t, second, "restore", ".gitignore")
	result, err = ApplyGitSync(ctx, second, plan.Head, plan.UpstreamHead, plan.PlanToken)
	if err != nil || result.State != "equal" {
		t.Fatal(result, err)
	}
	// A previously ignored artifact must survive a remote commit that tracks its name.
	makeFile(t, first, "ignored.bin", "remote version")
	fixtureGit(t, first, "add", "-f", "ignored.bin")
	fixtureGit(t, first, "commit", "-qm", "published asset")
	fixtureGit(t, first, "push", "-q")
	makeFile(t, second, "ignored.bin", "local asset")
	plan, err = PreviewGitSync(ctx, second)
	if err != nil || plan.State != "behind" {
		t.Fatal(plan, err)
	}
	if _, err = ApplyGitSync(ctx, second, plan.Head, plan.UpstreamHead, plan.PlanToken); err == nil {
		t.Fatal("overwrote ignored local file")
	}
	content, err := os.ReadFile(filepath.Join(second, "ignored.bin"))
	if err != nil || string(content) != "local asset" {
		t.Fatal(string(content), err)
	}
	fixtureGit(t, second, "commit", "--allow-empty", "-qm", "different")
	plan, err = PreviewGitSync(ctx, second)
	if err != nil || plan.State != "diverged" || plan.PlanToken != "" {
		t.Fatal(plan, err)
	}
}
