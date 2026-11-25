package server

import (
	"bytes"
	"encoding/json"
	"kirius.local/kiriusdrive/internal/objectfmt"
	"os"
	"testing"
)

func TestRebuildRepairsReachabilityOrRollsBack(t *testing.T) {
	f := timelineFixtureAt(t)
	a := f.add("", "A", "published")
	b := f.add(a.id, "B", "published")
	c := f.add(b.id, "C", "published")
	d := f.add(c.id, "D", "published")
	in := fields{"snapshotId": b.id, "expectedHead": d.id}
	raw, _ := f.request("POST", f.path+"/timeline/delete/preview", f.writer, in, 200)
	var plan deletionPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Points) != 1 || plan.Points[0].Snapshot != c.id || plan.Points[0].Before[0] != b.id || plan.Points[0].After[0] != a.id {
		t.Fatal(plan)
	}
	in["planToken"] = plan.Token
	location := f.s.path(objectfmt.Ref{Kind: "chunk", Digest: hash([]byte("C"))})
	if err := os.Rename(location, location+".saved"); err != nil {
		t.Fatal(err)
	}
	f.request("POST", f.path+"/timeline/delete", f.writer, in, 500)
	f.request("GET", f.path+"/snapshots/"+b.id, f.reader, nil, 200)
	raw, _ = f.request("GET", f.path+"/snapshots/"+c.id, f.writer, nil, 200)
	if parse(t, raw)["parentSnapshotId"] != b.id {
		t.Fatal("partial update after failed verification")
	}
	if err := os.Rename(location+".saved", location); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("DELETE FROM refs WHERE unit=? AND snapshot=?", f.unit, c.id); err != nil {
		t.Fatal(err)
	}
	f.request("POST", f.path+"/timeline/delete", f.writer, in, 200)
	raw, _ = f.request("GET", f.path+"/snapshots/"+c.id, f.writer, nil, 200)
	got := parse(t, raw)
	if got["rootDigest"] != c.root || got["parentSnapshotId"] != a.id || got["timelineRebuilt"] != true {
		t.Fatal(got)
	}
	body, _ := f.request("GET", f.path+"/snapshots/"+c.id+"/objects/"+hash([]byte("C"))+"?kind=chunk", f.writer, nil, 200)
	if !bytes.Equal(body, []byte("C")) {
		t.Fatal("unrestorable checkpoint")
	}
	raw, _ = f.request("GET", f.path+"/snapshots/"+d.id, f.writer, nil, 200)
	if parse(t, raw)["parentSnapshotId"] != c.id {
		t.Fatal("unrelated descendant changed")
	}
	// Removing the new visible first parent contracts once again into a full independent root.
	f.del(a.id, d.id, 200)
	raw, _ = f.request("GET", f.path+"/snapshots/"+c.id, f.writer, nil, 200)
	if parse(t, raw)["parentSnapshotId"] != nil {
		t.Fatal("dangling parent after root removal")
	}
}
