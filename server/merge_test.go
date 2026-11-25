package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestMergeTwoParentsRestartAndScope(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	web := httptest.NewTLSServer(s.Handler())
	site := testSite{t, web}
	unit, e := s.AddUnit("merge")
	if e != nil {
		t.Fatal(e)
	}
	other, e := s.AddUnit("private")
	if e != nil {
		t.Fatal(e)
	}
	writer, _ := site.enroll(s, unit, "sync")
	root, e := objectfmt.BuildDir(nil, s.install)
	if e != nil {
		t.Fatal(e)
	}
	add := func(unit, outcome string, parent *string) string {
		t.Helper()
		id := newID()
		raw, e := json.Marshal(fields{"snapshotId": id, "unitId": unit, "outcome": outcome, "rootDigest": root, "parentSnapshotId": parent, "attachments": []asset{}})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.db.Exec("INSERT INTO snapshots(unit,id,root,record) VALUES(?,?,?,?)", unit, id, root, raw); e != nil {
			t.Fatal(e)
		}
		if outcome == "published" {
			if _, e = s.db.Exec("UPDATE units SET head=? WHERE id=?", id, unit); e != nil {
				t.Fatal(e)
			}
		}
		return id
	}
	foreign := add(other, "fork", nil)
	a := add(unit, "published", nil)
	branch := add(unit, "fork", nil)
	path := "/units/" + unit + "/merge/"
	site.request("POST", path+"plan", writer, fields{"branchSnapshotId": foreign}, 404)
	body, _ := site.request("POST", path+"plan", writer, fields{"branchSnapshotId": branch}, 200)
	plan := parse(t, body)
	if plan["baseSnapshotId"] != "" || plan["alreadyMerged"] != false {
		t.Fatal(plan)
	}
	input := fields{"mainHead": a, "branchSnapshotId": branch, "requestId": newID(), "choices": []mergeChoice{}, "label": "merge roots"}
	body, _ = site.request("POST", path+"commit", writer, input, 200)
	merged := parse(t, body)["snapshotId"].(string)
	next := add(unit, "fork", &branch)
	body, _ = site.request("POST", path+"plan", writer, fields{"branchSnapshotId": next}, 200)
	plan = parse(t, body)
	if plan["baseSnapshotId"] != branch || plan["mainHead"] != merged || plan["alreadyMerged"] != false {
		t.Fatal("second-parent DAG edge not followed", plan)
	}
	web.Close()
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	web = httptest.NewTLSServer(s.Handler())
	defer web.Close()
	site = testSite{t, web}
	repeated, _ := site.request("POST", path+"commit", writer, input, 200)
	if parse(t, repeated)["snapshotId"] != merged {
		t.Fatal("durable replay changed ID")
	}
	body, _ = site.request("POST", path+"plan", writer, fields{"branchSnapshotId": branch}, 200)
	if parse(t, body)["alreadyMerged"] != true {
		t.Fatal("merged parent not reachable after restart")
	}
}
