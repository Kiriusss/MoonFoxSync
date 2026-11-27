package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestWorkspacePreviewSeparatesTrackedChangedUnbackedAndOnline(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	runGit := func(dir string, args ...string) {
		t.Helper()
		if out, e := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); e != nil {
			t.Fatalf("%s %v", out, e)
		}
	}
	runGit(f.source, "init", "-q")
	base := filepath.Join(f.source, "笔记")
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	makeFile(t, f.source, ".gitignore", "*.bin\n")
	for _, name := range []string{"tracked.txt", "修改.txt", "存档.bin", "仍在本机.bin"} {
		makeFile(t, base, name, name)
	}
	runGit(f.source, "add", "--", "笔记/tracked.txt")
	version, err := f.s.Snapshot(ctx, f.unit, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(ctx, f.unit, version.ID); err != nil {
		t.Fatal(err)
	}
	online := filepath.Join(base, "存档.bin")
	a, err := f.s.workspacePlan(ctx, online)
	if err != nil {
		t.Fatal(err)
	}
	if a.Generation == "" {
		t.Fatal("missing generation")
	}
	a.Slot = newID()
	a.State = "online"
	raw, _ := json.Marshal(a.Entry)
	if _, err = f.s.db.Exec("INSERT INTO workspace_archives("+workspaceColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", a.Slot, a.Unit, a.Snapshot, a.Member, a.Source, a.SHA, a.Size, a.Device, a.State, a.Generation, raw, a.Directory); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(online); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(f.s.workspaceTarget(a), online); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ch := filepath.Join(base, "修改.txt")
	if err = os.WriteFile(ch, bytes.Repeat([]byte("x"), len("修改.txt")), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(ch, time.Now(), time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	makeFile(t, base, "尚未上传.txt", "pending")
	// The binding now points at a queued child; unchanged members may use
	// their previous published ancestor while changed/new entries are skipped.
	if _, err = f.s.Snapshot(ctx, f.unit, "queued child"); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(base, "nested")
	if err = os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(nested, "init", "-q")
	// Count the normal entry point's Git invocations, without timing assertions.
	var counter string
	if runtime.GOOS != "windows" {
		git, _ := exec.LookPath("git")
		temp := t.TempDir()
		counter = filepath.Join(temp, "calls")
		if err = os.WriteFile(filepath.Join(temp, "git"), []byte("#!/bin/sh\nprintf x >> \"$PLAN_TEST_CALLS\"\nexec \"$PLAN_TEST_GIT\" \"$@\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PLAN_TEST_CALLS", counter)
		t.Setenv("PLAN_TEST_GIT", git)
		t.Setenv("PATH", temp+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	// GUI/CLI entry points can inherit another Git command's environment.
	// It must not redirect this safety check to an empty index.
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "unrelated-index"))
	p, err := f.s.WorkspacePlan(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if counter != "" {
		raw, _ := os.ReadFile(counter)
		if string(raw) != "x" {
			t.Fatalf("%d git invocations", len(raw))
		}
	}
	if len(p.Items) != 2 || len(p.Skipped) != 4 {
		t.Fatalf("items=%+v skipped=%v", p.Items, p.Skipped)
	}
	diag := strings.Join(p.Skipped, "\n")
	for _, hint := range []string{"Git 已跟踪", "文件修改时间", "未备份", "嵌套 Git"} {
		if !strings.Contains(diag, hint) {
			t.Fatal(hint, diag)
		}
	}
	states := map[string]string{}
	for _, item := range p.Items {
		states[filepath.Base(item.Member)] = item.State
	}
	if states["存档.bin"] != "online" || states["仍在本机.bin"] != "local" {
		t.Fatal(states)
	}
	for _, item := range p.Items {
		if item.Snapshot != version.ID || item.Unit != f.unit || item.Generation != a.Generation {
			t.Fatal(item)
		}
	}
	if _, err = f.s.db.Exec("UPDATE workspace_archives SET generation='stale' WHERE slot=?", a.Slot); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.WorkspacePlan(ctx, online); err == nil || !strings.Contains(err.Error(), "绑定") {
		t.Fatalf("stale generation: %v", err)
	}
	if runtime.GOOS != "windows" {
		alias := filepath.Join(t.TempDir(), "redirect")
		if err = os.Symlink(base, alias); err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.WorkspacePlan(ctx, filepath.Join(alias, "仍在本机.bin")); err == nil {
			t.Fatal("followed a parent symlink")
		}
	}
	if runtime.GOOS == "darwin" && strings.HasPrefix(a.Source, "/private/var/") {
		alias := strings.TrimPrefix(a.Source, "/private")
		canonical, err := canonicalWorkspacePath(alias)
		if err != nil || canonical != a.Source {
			t.Fatalf("system alias %s %v", canonical, err)
		}
	}
}
func TestWorkspaceGitRecognizesFilesystemAliases(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS original-path release")
	}
	dir := t.TempDir()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	run := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "-q")
	const actual, indexed = "café.txt", "CAFE\u0301.TXT"
	makeFile(t, dir, actual, "same")
	file, err := os.Stat(filepath.Join(dir, actual))
	if err != nil {
		t.Fatal(err)
	}
	alias, err := os.Stat(filepath.Join(dir, indexed))
	if err != nil || !os.SameFile(file, alias) {
		t.Skip("volume has distinct names")
	}
	hash := run("same", "hash-object", "-w", "--stdin")
	run("", "update-index", "--add", "--cacheinfo", "100644", hash, indexed)
	for _, folder := range []bool{false, true} {
		tracked, err := trackedWorkspacePaths(t.Context(), dir, folder)
		if err != nil {
			t.Fatal(err)
		}
		if !tracked.contains(filepath.Join(dir, actual), file) {
			t.Fatalf("missed Git identity of alias in folder=%v", folder)
		}
	}
}
func TestWorkspaceIndexDecodesWideDirectoryOnce(t *testing.T) {
	var refs = map[objectfmt.Ref][]byte{}
	put := func(r objectfmt.Ref, b []byte) error { refs[r] = append([]byte(nil), b...); return nil }
	file, err := objectfmt.BuildFile(strings.NewReader("same contents"), put)
	if err != nil {
		t.Fatal(err)
	}
	var members []objectfmt.Entry
	for i := 0; i < 1500; i++ {
		members = append(members, objectfmt.Entry{Name: fmt.Sprintf("%04d.bin", i), Kind: "file", Digest: file})
	}
	root, err := objectfmt.BuildDir(members, put)
	if err != nil {
		t.Fatal(err)
	}
	loads := map[objectfmt.Ref]int{}
	idx := workspaceIndex{load: func(r objectfmt.Ref) ([]byte, error) { loads[r]++; return refs[r], nil }, dirs: map[string][]objectfmt.Entry{}, files: map[string]workspaceDigest{}}
	for _, item := range members {
		v, e := idx.entry(root, item.Name)
		if e != nil {
			t.Fatal(e)
		}
		d, e := idx.content(v.Digest)
		if e != nil || d.size != 13 {
			t.Fatal(d, e)
		}
	}
	for r, count := range loads {
		if count != 1 {
			t.Fatalf("%v loaded %d times", r, count)
		}
	}
}

func TestWorkspaceIndexRejectsModifiedTree(t *testing.T) {
	f := setup(t)
	makeFile(t, f.source, "document", "saved")
	v := publishedWorkspaceCapture(t, f)
	path := f.s.objectPath(objectfmt.Ref{Kind: "tree", Digest: v.Root})
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A modified file digest still forms a valid directory page at the same size.
	page[len(page)-1] ^= 1
	if err := os.WriteFile(path, page, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.entryInSnapshot(v.Root, "document"); !errors.Is(err, objectfmt.ErrObjectIntegrity) {
		t.Fatalf("corrupt project metadata passed verification: %v", err)
	}
}
