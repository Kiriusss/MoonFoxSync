package server

import (
	"bytes"
	"testing"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestCollapseAutomaticHeadAndRedirectOnlyUntilNextPublish(t *testing.T) {
	f := timelineFixtureAt(t)
	// Register this unit as a folder project so Finder metadata is incidental.
	var scope string
	if err := f.s.db.QueryRow("SELECT id FROM units WHERE id<>? LIMIT 1", f.unit).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("INSERT INTO projects VALUES(?,?,?)", f.unit, scope, "fixture"); err != nil {
		t.Fatal(err)
	}
	a := f.add("", "data", "published")
	cosmetic := func(d *draft, save objectfmt.Store) {
		d.Label = ""
		sidecar, err := objectfmt.BuildFile(bytes.NewBufferString(d.ID), save)
		if err != nil {
			t.Fatal(err)
		}
		d.Root, err = objectfmt.BuildDir([]objectfmt.Entry{
			{Name: "data.txt", Kind: "file", Digest: d.Attachments[0].FileDigest, MtimeNS: 123},
			{Name: ".DS_Store", Kind: "file", Digest: sidecar},
		}, save)
		if err != nil {
			t.Fatal(err)
		}
	}
	b := f.addDraft(a.id, "data", "published", cosmetic)
	c := f.addDraft(b.id, "data", "published", cosmetic)
	ctx := t.Context()
	ids := []string{c.id, b.id}
	check := func(expected string, visible int) {
		t.Helper()
		var head string
		var n int
		if err := f.s.db.QueryRow("SELECT head,(SELECT count(*) FROM visible_snapshots WHERE unit=?1) FROM units WHERE id=?1", f.unit).Scan(&head, &n); err != nil || head != expected || n != visible {
			t.Fatalf("head=%v visible=%d err=%v", head, n, err)
		}
	}
	if out, err := f.s.collapseHeads(ctx, f.unit, c.id, a.id, ids, false); err != nil || out.Applied {
		t.Fatal(out, err)
	}
	check(c.id, 3)
	if _, err := f.s.collapseHeads(ctx, f.unit, c.id, a.id, []string{c.id}, true); err == nil {
		t.Fatal("incomplete chain")
	}
	if _, err := f.s.collapseHeads(ctx, f.unit, b.id, a.id, []string{b.id}, true); err == nil {
		t.Fatal("stale expected head")
	}
	check(c.id, 3)
	hold := f.path + "/holds/" + newID()
	f.request("PUT", hold, f.writer, fields{"snapshotId": b.id, "path": "data.txt"}, 200)
	if _, err := f.s.collapseHeads(ctx, f.unit, c.id, a.id, ids, true); err == nil {
		t.Fatal("removed active hold")
	}
	check(c.id, 3)
	f.request("DELETE", hold, f.writer, nil, 204)
	preserved := f.path + "/holds/" + newID()
	f.request("PUT", preserved, f.writer, fields{"snapshotId": a.id, "path": "data.txt"}, 200)
	if out, err := f.s.collapseHeads(ctx, f.unit, c.id, a.id, ids, true); err != nil || !out.Applied {
		t.Fatal(out, err)
	}
	check(a.id, 1)
	f.request("GET", f.path+"/snapshots/"+a.id, f.reader, nil, 200)
	f.request("GET", f.path+"/snapshots/"+b.id, f.reader, nil, 404)
	var held, objects int
	if err := f.s.db.QueryRow("SELECT count(*) FROM holds WHERE unit=? AND snapshot=? AND released=0", f.unit, a.id).Scan(&held); err != nil || held != 1 {
		t.Fatal(held, err)
	}
	if err := f.s.db.QueryRow("SELECT count(*) FROM refs WHERE unit=? AND snapshot=?", f.unit, c.id).Scan(&objects); err != nil || objects == 0 {
		t.Fatal("removed physical objects", err)
	}
	// Another device may have queued a change against an old, proven base.
	next := f.add(c.id, "actual edit", "published")
	check(next.id, 2)
	raw, _ := f.request("GET", f.path+"/snapshots/"+next.id, f.reader, nil, 200)
	if got := parse(t, raw)["parentSnapshotId"]; got != a.id {
		t.Fatal("visible parent", got)
	}
	f.add(b.id, "concurrent edit", "fork")
	check(next.id, 3)
}

func TestCollapseRefusesMeaningfulOrExplicitPoints(t *testing.T) {
	for _, kind := range []string{"label", "checkpoint", "content", "attachments", "git", "link-target", "executable", "merge", "fork", "standalone Finder file"} {
		t.Run(kind, func(t *testing.T) {
			f := timelineFixtureAt(t)
			a := f.add("", "before", "published")
			b := f.addDraft(a.id, "before", "published", func(d *draft, save objectfmt.Store) {
				d.Label = ""
				entry := objectfmt.Entry{Name: "data.txt", Kind: "file", Digest: d.Attachments[0].FileDigest}
				switch kind {
				case "label":
					d.Label = "my checkpoint"
				case "checkpoint":
					d.Checkpoint = true
				case "content":
					digest, err := objectfmt.BuildFile(bytes.NewBufferString("actual edit"), save)
					if err != nil {
						t.Fatal(err)
					}
					entry.Digest = digest
				case "attachments":
					d.Attachments = []asset{}
				case "git":
					d.GitBranch = "different"
				case "link-target":
					entry.Kind = "symlink"
					entry.Target = "elsewhere"
					entry.Digest = ""
				case "executable":
					entry.Executable = true
				}
				entries := []objectfmt.Entry{entry}
				if kind == "standalone Finder file" {
					entries = append(entries, objectfmt.Entry{Name: ".DS_Store", Kind: "file", Digest: entry.Digest})
				}
				var err error
				d.Root, err = objectfmt.BuildDir(entries, save)
				if err != nil {
					t.Fatal(err)
				}
			})
			if kind == "merge" || kind == "fork" {
				// Merge/fork publication metadata is server-generated.
				if kind == "merge" {
					_, err := f.s.db.Exec("UPDATE snapshots SET record=json_set(record,'$.mergeParentSnapshotId',?) WHERE id=?", newID(), b.id)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					_, err := f.s.db.Exec("UPDATE snapshots SET record=json_set(record,'$.outcome','fork') WHERE id=?", b.id)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.s.collapseHeads(t.Context(), f.unit, b.id, a.id, []string{b.id}, true); err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}
