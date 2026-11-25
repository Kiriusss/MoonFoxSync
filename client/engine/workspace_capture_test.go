//go:build darwin || linux

package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

// Model the durable post-release state. A dangling link deliberately models a
// disconnected mount and proves that capturing does not hydrate the archive.
func detachedWorkspaceFile(t *testing.T, f fixture, member string) WorkspaceFile {
	t.Helper()
	b, err := f.s.binding(t.Context(), f.unit)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(boundDirectory(b.Path, b.Profile), filepath.FromSlash(member))
	a, err := f.s.workspacePlan(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	a.Slot = newID()
	raw, err := json.Marshal(a.Entry)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.db.Exec("INSERT INTO workspace_archives("+workspaceColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		a.Slot, a.Unit, a.Snapshot, a.Member, a.Source, a.SHA, a.Size, a.Device, "online", a.Generation, raw, a.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.requireWorkspaceHold(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(a.Source); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(f.s.workspaceTarget(a), a.Source); err != nil {
		t.Fatal(err)
	}
	return a
}

func publishedWorkspaceCapture(t *testing.T, f fixture) Snapshot {
	t.Helper()
	v, err := f.s.Snapshot(t.Context(), f.unit, "version")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(t.Context(), f.unit, v.ID); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWorkspaceCaptureStandaloneFileOffline(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var device string
	if err := f.s.db.QueryRow("SELECT device FROM account").Scan(&device); err != nil {
		t.Fatal(err)
	}
	unit, err := f.nas.AddUnit("standalone")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.nas.Grant(unit, device, "sync"); err != nil {
		t.Fatal(err)
	}
	f.unit, f.source = unit, t.TempDir()
	const member = "report.txt"
	makeFile(t, f.source, member, "offline report")
	if _, err := f.s.Bind(ctx, f.unit, filepath.Join(f.source, member), "file", false, ""); err != nil {
		t.Fatal(err)
	}
	before := publishedWorkspaceCapture(t, f)
	a := detachedWorkspaceFile(t, f, member)
	after := publishedWorkspaceCapture(t, f)
	if after.Root != before.Root {
		t.Fatalf("standalone archive changed content: %s vs %s", after.Root, before.Root)
	}
	entry, err := f.s.entryInSnapshot(after.Root, member)
	if err != nil || entry != a.Entry {
		t.Fatalf("bad standalone entry: %+v, %v", entry, err)
	}
}

func TestWorkspaceCaptureKeepsPinnedFileAndHonorsGitSelection(t *testing.T) {
	ctx := t.Context()
	f := setup(t)
	fixtureGit(t, f.source, "init", "-q")
	const member = "originals/画册 & 素材.bin"
	makeFile(t, f.source, ".gitignore", "*.bin\n")
	makeFile(t, f.source, member, "before release")
	if err := os.Chmod(filepath.Join(f.source, member), 0755); err != nil {
		t.Fatal(err)
	}
	before := publishedWorkspaceCapture(t, f)
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := f.s.baselineFingerprint(ctx, b, defaultIgnored())
	if err != nil {
		t.Fatal(err)
	}
	a := detachedWorkspaceFile(t, f, member)
	// Only manifests remain locally. Excluding and reselecting this file
	// across successive heads must reuse the content retained by the NAS.
	content, err := objectfmt.ReadFile(a.Entry.Digest, f.s.load)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range content.Chunks {
		if err = os.Remove(f.s.objectPath(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.s.baselineFingerprint(ctx, b, defaultIgnored())
	if err != nil || fingerprint != after {
		t.Fatalf("content fingerprint changed: %s/%s, %v", fingerprint, after, err)
	}
	remote, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Capturing another project file must retain the original entry and refs.
	makeFile(t, f.source, "notes.md", "new edit")
	for _, policy := range []IgnoredPolicy{defaultIgnored(), {Mode: "selected", Items: []string{member}}} {
		if err := f.s.SetIgnoredPolicy(ctx, f.unit, policy); err != nil {
			t.Fatal(err)
		}
		v := publishedWorkspaceCapture(t, f)
		entry, err := f.s.entryInSnapshot(v.Root, member)
		if err != nil || entry != a.Entry {
			t.Fatalf("placeholder replaced original metadata: %+v %v", entry, err)
		}
		restored := filepath.Join(t.TempDir(), "restored")
		if _, err = remote.Restore(ctx, f.unit, v.ID, "", restored); err != nil {
			t.Fatal(err)
		}
		bytes, err := os.ReadFile(filepath.Join(restored, member))
		if err != nil || string(bytes) != "before release" {
			t.Fatalf("bad restore: %q %v", bytes, err)
		}
	}
	// Explicit exclusion still affects new snapshots. The original receipt
	// and durable mapping survive, allowing the item to be selected again.
	for _, policy := range []IgnoredPolicy{{Mode: "all", Items: []string{member}}, {Mode: "selected", Items: []string{}}} {
		if err := f.s.SetIgnoredPolicy(ctx, f.unit, policy); err != nil {
			t.Fatal(err)
		}
		v := publishedWorkspaceCapture(t, f)
		if _, err = f.s.entryInSnapshot(v.Root, member); err == nil {
			t.Fatal("excluded file was published")
		}
		saved, err := f.s.sourceRecord(ctx, a.Source)
		if err != nil || saved.Snapshot != before.ID {
			t.Fatal(saved, err)
		}
	}
	if err = f.s.SetIgnoredPolicy(ctx, f.unit, defaultIgnored()); err != nil {
		t.Fatal(err)
	}
	reselected := publishedWorkspaceCapture(t, f)
	if entry, err := f.s.entryInSnapshot(reselected.Root, member); err != nil || entry != a.Entry {
		t.Fatalf("reselecting archive lost pinned entry: %+v %v", entry, err)
	}
}

func TestWorkspaceCaptureRefusesMissingMovedOrReplacedReference(t *testing.T) {
	cases := []string{"missing", "ordinary replacement", "changed symlink", "replaced parent", "moved root", "nested repo", "wrong device", "new binding generation", "unfinished hydration", "copied managed reference", "excluded missing link"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			ctx := t.Context()
			makeFile(t, f.source, "folder/asset", "original")
			before := publishedWorkspaceCapture(t, f)
			a := detachedWorkspaceFile(t, f, "folder/asset")
			var err error
			switch kind {
			case "missing":
				err = os.Remove(a.Source)
			case "ordinary replacement":
				if err = os.Remove(a.Source); err == nil {
					err = os.WriteFile(a.Source, []byte("changed locally"), 0600)
				}
			case "changed symlink":
				if err = os.Remove(a.Source); err == nil {
					err = os.Symlink("some-other-file", a.Source)
				}
			case "replaced parent":
				parent := filepath.Dir(a.Source)
				if err = os.Rename(parent, parent+"-original"); err == nil {
					err = os.Mkdir(parent, 0700)
				}
				if err == nil {
					err = os.Symlink(f.s.workspaceTarget(a), a.Source)
				}
			case "moved root":
				err = os.Rename(f.source, filepath.Join(t.TempDir(), "moved"))
			case "nested repo":
				err = os.Mkdir(filepath.Join(filepath.Dir(a.Source), ".git"), 0700)
			case "wrong device":
				_, err = f.s.db.Exec("UPDATE workspace_archives SET device=?", newID())
			case "new binding generation":
				_, err = f.s.db.Exec("UPDATE workspace_archives SET generation=?", newID())
			case "unfinished hydration":
				_, err = f.s.db.Exec("UPDATE workspace_archives SET phase='hydrating'")
			case "copied managed reference":
				parent := filepath.Dir(a.Source)
				relative, relErr := filepath.Rel(parent, f.s.workspaceTarget(a))
				if relErr != nil {
					t.Fatal(relErr)
				}
				err = os.Symlink(relative, filepath.Join(parent, "copied"))
			case "excluded missing link":
				fixtureGit(t, f.source, "init", "-q")
				makeFile(t, f.source, ".gitignore", "folder/asset\n")
				err = f.s.SetIgnoredPolicy(ctx, f.unit, IgnoredPolicy{Mode: "all", Items: []string{a.Member}})
				if err == nil {
					err = os.Remove(a.Source)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.Snapshot(ctx, f.unit, "missing"); err == nil {
				t.Fatal("unsafe capture unexpectedly succeeded")
			}
			b, err := f.s.binding(ctx, f.unit)
			if err != nil || b.Base == nil || *b.Base != before.ID {
				t.Fatal("baseline advanced", b, err)
			}
			var count int
			if err = f.s.db.QueryRow("SELECT COUNT(*) FROM snapshots WHERE unit=?", f.unit).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}
