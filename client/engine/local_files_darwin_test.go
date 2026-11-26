//go:build darwin && cgo

package engine

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestDeleteLocalFilesLeavesNASAndUnbackedFiles(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, b.Path, "init", "-q")
	makeFile(t, b.Path, "documents/tracked.txt", "tracked")
	fixtureGit(t, b.Path, "add", "documents/tracked.txt")
	for _, p := range []string{"original", "changed", "archived", "resource", "legacyfork", "linked", "in-use"} {
		makeFile(t, b.Path, p, "initial")
	}
	before := publishedWorkspaceCapture(t, f)
	link := detachedWorkspaceFile(t, f, "archived")
	legacy := detachedWorkspaceFile(t, f, "legacyfork")
	if err = os.MkdirAll(filepath.Dir(f.s.metadataPath(legacy)), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.s.metadataPath(legacy), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = unix.Setxattr(f.s.metadataPath(legacy), "com.apple.ResourceFork", []byte("unique"), 0); err != nil {
		t.Fatal(err)
	}
	makeFile(t, b.Path, "changed", "altered")
	makeFile(t, b.Path, "new.txt", "new")
	makeFile(t, b.Path, "nested/.git/HEAD", "ref")
	makeFile(t, b.Path, "nested/private", "keep")
	if err = unix.Setxattr(filepath.Join(b.Path, "resource"), "com.apple.ResourceFork", []byte("unique"), 0); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(filepath.Join(b.Path, "linked"), filepath.Join(b.Path, "linked-too")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("original", filepath.Join(b.Path, "foreign-link")); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	calls := []string{}
	upstream := f.nas.Handler()
	web := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		upstream.ServeHTTP(w, r)
	}))
	defer web.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: web.Certificate().Raw})
	if _, err = f.s.db.Exec("UPDATE account SET url=?,ca=?", web.URL, ca); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.DeleteLocalFiles(ctx, f.unit, b.Path+"-wrong")
	if err == nil || r.Removed != 0 || r.Paused {
		t.Fatal(r, err)
	}
	// A separate process holds the file through the normal entry point.
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWorkspaceNativeFileHolder$")
	child.Env = append(os.Environ(), "MOONFOX_TEST_HOLDER_FILE="+filepath.Join(b.Path, "in-use"), "MOONFOX_TEST_HOLDER_MODE=read")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); child.Wait() }()
	var ready [1]byte
	if _, err = io.ReadFull(stdout, ready[:]); err != nil || ready[0] != 1 {
		t.Fatal("holder", err, ready)
	}
	r, err = f.s.DeleteLocalFiles(ctx, f.unit, b.Path)
	if err != nil || r.Removed != 3 || !r.Paused {
		t.Fatal(r, err)
	}
	mu.Lock()
	if len(calls) != 1 || calls[0] != "GET /v1/units/"+f.unit+"/snapshots/"+before.ID {
		t.Errorf("unexpected NAS requests: %v", calls)
	}
	mu.Unlock()
	for _, p := range []string{"original", "documents/tracked.txt", "archived"} {
		if _, err = os.Lstat(filepath.Join(b.Path, p)); !os.IsNotExist(err) {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{"changed", "new.txt", ".git", "nested/private", "resource", "linked", "linked-too", "in-use", "foreign-link", "legacyfork"} {
		if _, err = os.Lstat(filepath.Join(b.Path, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	var count int
	if err = f.s.db.QueryRow("SELECT count(*) FROM workspace_archives WHERE slot=?", link.Slot).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	remote, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hold struct {
		State string `json:"state"`
	}
	if err = remote.call(ctx, "GET", "/units/"+f.unit+"/holds/"+link.Slot, nil, nil, &hold); err != nil || hold.State != "active" {
		t.Fatal(hold, err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if _, err = remote.Restore(ctx, f.unit, before.ID, "", dest); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"original", "archived", "changed"} {
		raw, err := os.ReadFile(filepath.Join(dest, p))
		if err != nil || string(raw) != "initial" {
			t.Fatal(p, err)
		}
	}
}

// Recover an interrupted rename using the precise inode and recorded snapshot,
// then keep an unrelated new file placed at the original name.
func TestDeleteLocalFilesResumesHeldInode(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	makeFile(t, b.Path, "resume", "old")
	snap := publishedWorkspaceCapture(t, f)
	path := filepath.Join(b.Path, "resume")
	a, err := f.s.workspacePlan(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	i := deleteIntent{Member: a.Member, Snapshot: snap.ID, Directory: a.Directory, Stage: ".moonfox-delete-" + newID(), Inode: inodeKey(st)}
	if err = f.s.pauseLocalDeletion(ctx, b); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(i)
	if _, err = f.s.db.Exec("INSERT INTO local_delete_intents(unit,generation,intent) VALUES(?,?,?)", b.Unit, b.generation, raw); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path, filepath.Join(b.Path, i.Stage)); err != nil {
		t.Fatal(err)
	}
	makeFile(t, b.Path, "resume", "new")
	result, err := f.s.DeleteLocalFiles(ctx, f.unit, b.Path)
	if err != nil || result.Removed != 1 || result.Skipped != 1 {
		t.Fatal(result, err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != "new" {
		t.Fatal(err, string(bytes))
	}
	if _, err = os.Lstat(filepath.Join(b.Path, i.Stage)); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestDeleteLocalFilesOfflineAndChangedLeaveSourceActive(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	makeFile(t, b.Path, "changed", "old")
	publishedWorkspaceCapture(t, f)
	makeFile(t, b.Path, "changed", "new")
	result, err := f.s.DeleteLocalFiles(ctx, f.unit, b.Path)
	if err != nil || result.Removed != 0 || result.Skipped != 1 || result.Paused {
		t.Fatal(result, err)
	}
	if _, err = f.s.db.Exec("UPDATE account SET url='https://127.0.0.1:1'"); err != nil {
		t.Fatal(err)
	}
	makeFile(t, b.Path, "changed", "old")
	result, err = f.s.DeleteLocalFiles(ctx, f.unit, b.Path)
	if err == nil || result.Paused || result.Removed > 0 {
		t.Fatal(result, err)
	}
	data, err := os.ReadFile(filepath.Join(b.Path, "changed"))
	if err != nil || string(data) != "old" {
		t.Fatal(err, string(data))
	}
}

func TestDeleteLocalFilesStandaloneKeepsSiblings(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var device string
	if err := f.s.db.QueryRow("SELECT device FROM account").Scan(&device); err != nil {
		t.Fatal(err)
	}
	unit, err := f.nas.AddUnit("one file")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.nas.Grant(unit, device, "sync"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	makeFile(t, dir, "selected.bin", "backup")
	makeFile(t, dir, "sibling.bin", "backup")
	b, err := f.s.Bind(ctx, unit, filepath.Join(dir, "selected.bin"), "file", false, "")
	if err != nil {
		t.Fatal(err)
	}
	f.unit = unit
	v := publishedWorkspaceCapture(t, f)
	result, err := f.s.DeleteLocalFiles(ctx, unit, b.Path)
	if err != nil || result.Removed != 1 || result.Skipped != 0 || !result.Paused {
		t.Fatal(result, err)
	}
	if _, err = os.Lstat(b.Path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if bytes, err := os.ReadFile(filepath.Join(dir, "sibling.bin")); err != nil || string(bytes) != "backup" {
		t.Fatal(err)
	}
	again, err := f.s.DeleteLocalFiles(ctx, unit, b.Path)
	if err != nil || again.Removed != 0 || !again.Paused {
		t.Fatal(again, err)
	}
	r, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err = r.call(ctx, "GET", "/units/"+unit+"/snapshots/"+v.ID, nil, nil, &receipt); err != nil || receipt.Root != v.Root {
		t.Fatal(receipt, err)
	}
}

func TestDeleteLocalFilesUsesPublishedMetadataFromNAS(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	makeFile(t, b.Path, "remote-only.txt", "already on NAS")
	v := publishedWorkspaceCapture(t, f)
	// A freshly bound device may have only the remote base ID. Drop the fixture's
	// local metadata and snapshot (including references), leaving that base intact.
	if _, err = f.s.db.Exec("DELETE FROM snapshot_objects WHERE snapshot=?", v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec("DELETE FROM snapshots WHERE id=?", v.ID); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(f.s.objectPath(objectfmt.Ref{Kind: "tree", Digest: v.Root})); err != nil {
		t.Fatal(err)
	}
	result, err := f.s.DeleteLocalFiles(ctx, f.unit, b.Path)
	if err != nil || result.Removed != 1 || result.Skipped != 0 {
		t.Fatal(result, err)
	}
	if _, err = os.Lstat(filepath.Join(b.Path, "remote-only.txt")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestDeleteLocalFilesReadOnlyFolderPreservesPreviousPause(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("effective root can rename in a read-only directory")
	}
	f := setup(t)
	ctx := t.Context()
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	makeFile(t, b.Path, "readonly", "version")
	publishedWorkspaceCapture(t, f)
	if err = os.Chmod(b.Path, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(b.Path, 0700)
	for _, alreadyPaused := range []bool{false, true} {
		if alreadyPaused {
			if err = f.s.pauseLocalDeletion(ctx, b); err != nil {
				t.Fatal(err)
			}
		}
		result, err := f.s.DeleteLocalFiles(ctx, b.Unit, b.Path)
		if err != nil || result.Removed != 0 || result.Skipped != 1 || result.Paused != alreadyPaused {
			t.Fatal(result, err)
		}
		latest, err := f.s.binding(ctx, b.Unit)
		if err != nil || latest.LocalFilesDeleted != alreadyPaused {
			t.Fatal(latest, err)
		}
	}
}
