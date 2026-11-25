package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=fixture", "GIT_COMMITTER_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@localhost", "GIT_COMMITTER_EMAIL=fixture@localhost", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}
func makeFile(t *testing.T, dir, rel, contents string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(contents), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestDiscoveryAndOriginIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	makeFile(t, root, "mono/go.mod", "module sample")
	makeFile(t, root, "mono/packages/one/package.json", "{}")
	makeFile(t, root, "assorted/nested/research/pyproject.toml", "")
	makeFile(t, root, "ordinary/readme.txt", "x")
	makeFile(t, root, ".hidden/go.mod", "x")
	makeFile(t, root, "node_modules/drop/package.json", "x")
	git := filepath.Join(root, "repo")
	if e := os.Mkdir(git, 0700); e != nil {
		t.Fatal(e)
	}
	fixtureGit(t, git, "init", "-q", "-b", "main")
	fixtureGit(t, git, "commit", "--allow-empty", "-qm", "first")
	fixtureGit(t, git, "remote", "add", "origin", "https://user:secret@github.com/Owner/Package.git?pass=secret#private")
	fixtureGit(t, git, "worktree", "add", "-q", "-b", "feature", filepath.Join(root, "other"))
	if e := os.Symlink(git, filepath.Join(root, "link")); e != nil && runtime.GOOS != "windows" {
		t.Fatal(e)
	}
	fixtureGit(t, root, "init", "-q", "--bare", filepath.Join(root, "private.git"))
	result, e := DiscoverProjects(ctx, root)
	if e != nil {
		t.Fatal(e)
	}
	var names []string
	var origin string
	for _, p := range result.Items {
		names = append(names, p.Name)
		if p.Kind == "git" {
			if p.Origin != "github.com/owner/package" || p.SourceKey == "" {
				t.Fatal(p)
			}
			if origin != "" && origin != p.SourceKey {
				t.Fatal("worktree identity")
			}
			origin = p.SourceKey
		}
	}
	if !slices.Equal(names, []string{"research", "mono", "ordinary", "other", "repo"}) {
		t.Fatal(names)
	}
	for _, alias := range []string{"ssh://git@github.com:22/Owner/Package.git", "git@github.com:OWNER/PACKAGE.git", "https://github.com:443/Owner/Package/"} {
		key, desc := originIdentity(git, alias)
		if key != origin || strings.Contains(desc, "secret") {
			t.Fatal(alias, key, desc)
		}
	}
	key, _ := originIdentity(git, "git@github.com:Else/Package.git")
	if key == origin {
		t.Fatal("unrelated same name collided")
	}
	local := filepath.Join(root, "remote.git")
	a, disp := originIdentity(git, local)
	b, _ := originIdentity(git, "file:///"+strings.TrimPrefix(filepath.ToSlash(local), "/"))
	if a != b || a == "" || strings.Contains(disp, root) {
		t.Fatal("local remotes")
	}
}

func TestGitUpstreamFreshStatesLeaveRefsAlone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "upstream.git")
	fixtureGit(t, root, "init", "-q", "--bare", "-b", "main", bare)
	fixtureGit(t, root, "clone", "-q", bare, "left")
	left := filepath.Join(root, "left")
	check := func(dir, state string, dirty bool, ahead, behind int) {
		t.Helper()
		s, e := InspectGit(ctx, dir, true)
		if e != nil || s.State != state || s.Dirty != dirty || s.Ahead != ahead || s.Behind != behind {
			t.Fatalf("expected %s dirty=%v %d/%d got %+v %v", state, dirty, ahead, behind, s, e)
		}
	}
	check(left, "unborn", false, 0, 0)
	makeFile(t, left, "x", "one")
	fixtureGit(t, left, "add", ".")
	fixtureGit(t, left, "commit", "-qm", "base")
	fixtureGit(t, left, "push", "-qu", "origin", "main")
	fixtureGit(t, root, "clone", "-q", bare, "right")
	right := filepath.Join(root, "right")
	check(left, "equal", false, 0, 0)
	makeFile(t, right, "new", "two")
	fixtureGit(t, right, "add", ".")
	fixtureGit(t, right, "commit", "-qm", "remote")
	fixtureGit(t, right, "push", "-q")
	remoteHead := fixtureGit(t, right, "rev-parse", "HEAD")
	light, err := InspectGitLight(ctx, left)
	if err != nil || light.State != "remote_changed" || !light.Fetched || light.UpstreamHead != remoteHead {
		t.Fatal("lightweight upstream inspection", light, err)
	}
	if _, err = gitValue(ctx, left, "cat-file", "-e", remoteHead+"^{commit}"); err == nil {
		t.Fatal("automatic check downloaded remote objects")
	}
	oldRefs := fixtureGit(t, left, "show-ref")
	idx := filepath.Join(left, ".git", "index")
	indexBefore, e := os.ReadFile(idx)
	if e != nil {
		t.Fatal(e)
	}
	check(left, "behind", false, 0, 1)
	if fixtureGit(t, left, "show-ref") != oldRefs {
		t.Fatal("inspecting changed local refs")
	}
	indexAfter, _ := os.ReadFile(idx)
	if sha256.Sum256(indexBefore) != sha256.Sum256(indexAfter) {
		t.Fatal("index changed")
	}
	if _, e := os.Stat(filepath.Join(left, ".git", "FETCH_HEAD")); !os.IsNotExist(e) {
		t.Fatal("FETCH_HEAD written")
	}
	makeFile(t, left, "local", "pending")
	check(left, "behind", true, 0, 1)
	fixtureGit(t, left, "add", ".")
	fixtureGit(t, left, "commit", "-qm", "divergent")
	check(left, "diverged", false, 1, 1)
	fixtureGit(t, right, "commit", "-q", "--allow-empty", "-m", "local ahead")
	check(right, "ahead", false, 1, 0)
	fixtureGit(t, right, "switch", "-qc", "scratch")
	check(right, "no_upstream", false, 0, 0)
	fixtureGit(t, right, "switch", "-q", "--detach", "HEAD")
	check(right, "detached", false, 0, 0)
	fixtureGit(t, left, "remote", "set-url", "origin", filepath.Join(root, "missing.git"))
	check(left, "offline", false, 0, 0)
	check(root, "not_git", false, 0, 0)
}

func TestSelectIgnoredFileAcrossRestartAndChunkedRestore(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	fixtureGit(t, f.source, "init", "-qb", "main")
	makeFile(t, f.source, ".gitignore", "weights/\n.env\nlink.bin\n")
	makeFile(t, f.source, ".env", "private")
	makeFile(t, f.source, "weights/tiny.bin", "also ignored")
	makeFile(t, f.source, "main.go", "package main")
	p := filepath.Join(f.source, "weights", "w[1].bin")
	file, e := os.Create(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = file.Truncate(smallLimit + 1); e != nil {
		t.Fatal(e)
	}
	file.Close()
	symlink := filepath.Join(f.source, "link.bin")
	if e = os.Symlink(p, symlink); e != nil && runtime.GOOS != "windows" {
		t.Fatal(e)
	}
	files, e := IgnoredFiles(ctx, f.source, smallLimit)
	if e != nil || len(files) != 1 || files[0].Path != "weights/w[1].bin" {
		t.Fatal(files, e)
	}
	for _, bad := range []string{"main.go", "../outside", ".git/config", "weights"} {
		if e = f.s.SetIgnoredSelection(ctx, f.unit, []string{bad}); e == nil {
			t.Fatal("accepted", bad)
		}
	}
	if e = f.s.SetIgnoredSelection(ctx, f.unit, []string{files[0].Path}); e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(f.s.Home)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	paths, e := reopened.IgnoredSelection(ctx, f.unit)
	if e != nil || !reflect.DeepEqual(paths, []string{files[0].Path}) {
		t.Fatal(paths, e)
	}
	snap, e := reopened.Snapshot(ctx, f.unit, "with weights")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = reopened.AutoSync(ctx, snap, AutoPolicy{AwaySmall: true}); !errors.Is(e, ErrWaiting) {
		t.Fatal(e)
	}
	if _, e = reopened.Sync(ctx, f.unit, snap.ID); e != nil {
		t.Fatal(e)
	}
	remote, e := reopened.Remote(ctx)
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "restore")
	if _, e = remote.Restore(ctx, f.unit, snap.ID, "", dest); e != nil {
		t.Fatal(e)
	}
	a, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(dest, files[0].Path))
	if e != nil || sha256.Sum256(a) != sha256.Sum256(b) {
		t.Fatal("restore", e)
	}
	for _, rel := range []string{"weights/tiny.bin", ".env", "link.bin"} {
		if _, e = os.Lstat(filepath.Join(dest, rel)); !os.IsNotExist(e) {
			t.Fatal("included", rel)
		}
	}
	if e = f.s.SetIgnoredSelection(ctx, f.unit, nil); e != nil {
		t.Fatal(e)
	}
	if cleared, err := f.s.IgnoredSelection(ctx, f.unit); err != nil || len(cleared) != 0 {
		t.Fatal(cleared, err)
	}
	if err := os.Rename(filepath.Join(f.source, ".git"), filepath.Join(t.TempDir(), "metadata")); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Snapshot(ctx, f.unit, "missing git marker"); err == nil {
		t.Fatal("missing metadata exposed ignored files")
	}
}

func TestDefaultIgnoredPolicyBacksUpCurrentAndFutureData(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	fixtureGit(t, f.source, "init", "-qb", "main")
	makeFile(t, f.source, ".gitignore", "*.bin\n.env\n")
	makeFile(t, f.source, ".env", "hidden")
	makeFile(t, f.source, "skip.bin", "excluded")
	makeFile(t, f.source, "tiny.bin", "small")
	makeFile(t, f.source, "dir/deep.bin", "nested")
	if err := os.Mkdir(filepath.Join(f.source, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	fresh := func(want []string, absent []string) {
		t.Helper()
		v, err := f.s.Snapshot(ctx, f.unit, "policy")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.Sync(ctx, f.unit, v.ID); err != nil {
			t.Fatal(err)
		}
		r, err := f.s.Remote(ctx)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(t.TempDir(), "restored")
		if _, err = r.Restore(ctx, f.unit, v.ID, "", dst); err != nil {
			t.Fatal(err)
		}
		for _, p := range want {
			if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
				t.Fatal(p, err)
			}
		}
		for _, p := range absent {
			if _, err := os.Lstat(filepath.Join(dst, p)); !os.IsNotExist(err) {
				t.Fatal(p, err)
			}
		}
	}
	fresh([]string{".env", "skip.bin", "tiny.bin", "dir/deep.bin", "empty"}, []string{".git"})
	if err := f.s.SetIgnoredPolicy(ctx, f.unit, IgnoredPolicy{Mode: "all", Items: []string{"skip.bin"}}); err != nil {
		t.Fatal(err)
	}
	makeFile(t, f.source, "future.bin", "new")
	fresh([]string{"future.bin", ".env", "dir/deep.bin"}, []string{"skip.bin", ".git"})
	fixtureGit(t, f.source, "add", "-f", "skip.bin")
	fresh([]string{"skip.bin", "future.bin"}, []string{".git"})
	// Check an installation upgraded from an explicit 0.3.0 allowlist.
	if _, err := f.s.db.Exec("UPDATE ignored_selection SET paths='[]' WHERE unit=?", f.unit); err != nil {
		t.Fatal(err)
	}
	p, err := f.s.IgnoredPolicy(ctx, f.unit)
	if err != nil || p.Mode != "selected" || len(p.Items) != 0 {
		t.Fatal(p, err)
	}
	fresh([]string{"skip.bin"}, []string{"future.bin", ".env"})
}
