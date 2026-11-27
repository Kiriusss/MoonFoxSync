package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

type timelineFixture struct {
	testSite
	s                                              *Store
	storeDir, unit, path, writer, reader, stranger string
}

func timelineFixtureAt(t *testing.T) timelineFixture {
	t.Helper()
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewTLSServer(s.Handler())
	t.Cleanup(func() { web.Close(); s.Close() })
	site := testSite{t, web}
	unit, err := s.AddUnit("timeline")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.AddUnit("private")
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := site.enroll(s, unit, "sync")
	reader, _ := site.enroll(s, unit, "reader")
	stranger, _ := site.enroll(s, other, "sync")
	return timelineFixture{site, s, dir, unit, "/units/" + unit, writer, reader, stranger}
}

type testPoint struct {
	id, root, session string
}

func (f timelineFixture) add(parent, body, want string) testPoint {
	f.t.Helper()
	return f.addDraft(parent, body, want, nil)
}

func (f timelineFixture) addDraft(parent, body, want string, edit func(*draft, objectfmt.Store)) testPoint {
	t := f.t
	t.Helper()
	id := newID()
	var p *string
	if parent != "" {
		p = &parent
	}
	blobs := map[objectfmt.Ref][]byte{}
	save := func(ref objectfmt.Ref, b []byte) error { blobs[ref] = bytes.Clone(b); return nil }
	file, err := objectfmt.BuildFile(bytes.NewBufferString(body), save)
	if err != nil {
		t.Fatal(err)
	}
	root, err := objectfmt.BuildDir([]objectfmt.Entry{{Name: "data.txt", Kind: "file", Digest: file}}, save)
	if err != nil {
		t.Fatal(err)
	}
	d := draft{RequestID: newID(), ID: id, Parent: p, Base: p, Root: root, Captured: stamp(time.Now().Add(-30 * 24 * time.Hour)), Revision: 1, Label: "test", Links: []json.RawMessage{}, Attachments: []asset{{"git-bundle", "git.bundle", file}}}
	if edit != nil {
		edit(&d, save)
	}
	b, _ := f.request("POST", f.path+"/uploads", f.writer, d, 201)
	session := "/uploads/" + parse(t, b)["uploadId"].(string)
	var offer []fields
	for ref, content := range blobs {
		offer = append(offer, fields{"kind": ref.Kind, "digest": ref.Digest, "size": len(content)})
	}
	b, _ = f.request("POST", session+"/missing", f.writer, fields{"objects": offer}, 200)
	var missing struct{ Missing []objectfmt.Ref }
	if err = json.Unmarshal(b, &missing); err != nil {
		t.Fatal(err)
	}
	var batch []fields
	for _, ref := range missing.Missing {
		batch = append(batch, fields{"kind": ref.Kind, "digest": ref.Digest, "data": blobs[ref]})
	}
	if len(batch) > 0 {
		f.request("POST", session+"/batch", f.writer, fields{"items": batch}, 200)
	}
	b, _ = f.request("POST", session+"/finalize", f.writer, nil, 200)
	if parse(t, b)["outcome"] != want {
		t.Fatal(string(b))
	}
	return testPoint{id, root, session}
}
func (f timelineFixture) del(id, head string, code int) {
	f.t.Helper()
	input := fields{"snapshotId": id, "expectedHead": head}
	if code == 200 {
		preview, _ := f.request("POST", f.path+"/timeline/delete/preview", f.writer, input, 200)
		input["planToken"] = parse(f.t, preview)["planToken"]
	}
	raw, _ := f.request("POST", f.path+"/timeline/delete", f.writer, input, code)
	if code == 200 {
		again, _ := f.request("POST", f.path+"/timeline/delete", f.writer, input, code)
		if !bytes.Equal(raw, again) {
			f.t.Fatal("different replayed deletion receipt")
		}
	}

}

func TestTimelineHideReadSurfacesAndProtectHolds(t *testing.T) {
	f := timelineFixtureAt(t)
	a := f.add("", "private old body", "published")
	b := f.add(a.id, "fresh", "published")
	c := f.add(b.id, "latest", "published")
	fork := f.add(a.id, "unmerged", "fork")
	hold := f.path + "/holds/" + newID()
	f.request("PUT", hold, f.writer, fields{"snapshotId": b.id, "path": "data.txt"}, 200)
	history, _ := f.request("GET", f.path+"/snapshots?limit=1", f.reader, nil, 200)
	cursor := parse(t, history)["nextCursor"].(string)
	units, _ := f.request("GET", "/units", f.reader, nil, 200)
	changes := parse(t, units)["syncCursor"].(string)
	f.del(a.id, b.id, 409)
	f.del(c.id, c.id, 409)
	f.del(b.id, c.id, 409)
	f.del(fork.id, c.id, 409)
	f.request("POST", f.path+"/timeline/delete", f.reader, fields{"snapshotId": a.id, "expectedHead": c.id}, 403)
	f.request("POST", f.path+"/timeline/delete", f.stranger, fields{"snapshotId": a.id, "expectedHead": c.id}, 404)
	f.del(a.id, c.id, 200)
	f.request("GET", f.path+"/snapshots?cursor="+cursor, f.reader, nil, 410)
	f.request("GET", "/changes?cursor="+changes, f.reader, nil, 410)
	for _, suffix := range []string{
		"/snapshots/" + a.id, "/snapshots/" + a.id + "?fingerprint=true", "/entries?snapshotId=" + a.id,
		"/content?snapshotId=" + a.id + "&path=data.txt", "/diff?from=" + a.id + "&to=" + c.id,
		"/merge/preview?snapshotId=" + a.id + "&path=data.txt", "/snapshots/" + a.id + "/objects/" + a.root + "?kind=tree", "/snapshots/" + a.id + "/attachments/git.bundle",
	} {
		f.request("GET", f.path+suffix, f.writer, nil, 404)
	}
	f.request("PUT", f.path+"/holds/"+newID(), f.writer, fields{"snapshotId": a.id, "path": "data.txt"}, 404)
	unchanged, _ := f.request("POST", a.session+"/finalize", f.writer, nil, 200)
	if parse(t, unchanged)["snapshotId"] != a.id {
		t.Fatal(string(unchanged))
	}
	// The deleted main/branch ancestor still provides an internal 3-way merge base.
	raw, _ := f.request("POST", f.path+"/merge/plan", f.reader, fields{"branchSnapshotId": fork.id}, 200)
	plan := parse(t, raw)
	if plan["baseSnapshotId"] != a.id || plan["baseHidden"] != true {
		t.Fatal(plan)
	}
	preview := f.path + "/history-policy/preview"
	f.request("POST", preview, f.reader, historyPolicy{"latest", 1}, 403)
	raw, _ = f.request("POST", preview, f.writer, historyPolicy{"latest", 1}, 200)
	res := parse(t, raw)
	if res["removeCount"] != float64(0) || res["protectedCount"] != float64(3) {
		t.Fatal(res)
	}
	f.request("PUT", f.path+"/history-policy", f.writer, historyPolicy{"latest", 1}, 200)
	f.request("DELETE", hold, f.writer, nil, 204)
	f.request("GET", f.path+"/snapshots/"+b.id, f.reader, nil, 404)
	f.request("GET", f.path+"/snapshots/"+fork.id, f.reader, nil, 200)
	d := f.add(c.id, "after policy", "published")
	f.request("GET", f.path+"/snapshots/"+c.id, f.reader, nil, 404)
	f.request("GET", f.path+"/snapshots/"+d.id, f.reader, nil, 200)
	ref := objectfmt.Ref{Kind: "tree", Digest: a.root}
	if _, err := f.s.read(ref); err != nil {
		t.Fatal("internal snapshot object lost", err)
	}
	// A digest from a hidden-only point no longer authorizes object reuse.
	draft := draft{ID: newID(), RequestID: newID(), Base: &d.id, Parent: &d.id, Root: a.root, Captured: stamp(time.Now()), Revision: 1, Attachments: []asset{}, Links: []json.RawMessage{}}
	raw, _ = f.request("POST", f.path+"/uploads", f.writer, draft, 201)
	session := "/uploads/" + parse(t, raw)["uploadId"].(string)
	tree, err := f.s.read(ref)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = f.request("POST", session+"/missing", f.writer, fields{"objects": []fields{{"kind": "tree", "digest": a.root, "size": len(tree)}}}, 200)
	if len(parse(t, raw)["missing"].([]any)) != 1 {
		t.Fatal("old root digest granted upload reuse")
	}
	f.request("POST", session+"/finalize", f.writer, nil, 409)
	// An offline device with the actual bytes may safely publish a fresh fork from
	// a removed parent; it cannot change the archived point or current main.
	offline := f.add(a.id, "private old body", "fork")
	f.request("GET", f.path+"/snapshots/"+offline.id, f.reader, nil, 200)
	f.request("GET", f.path+"/snapshots/"+a.id, f.reader, nil, 404)
}

func TestTimelineRestoreNewHeadAndDurableReplay(t *testing.T) {
	f := timelineFixtureAt(t)
	a := f.add("", "old body", "published")
	b := f.add(a.id, "new body", "published")
	endpoint := f.path + "/timeline/restore"
	in := fields{"snapshotId": a.id, "expectedHead": b.id, "requestId": newID(), "label": "恢复"}
	f.request("POST", endpoint, f.reader, in, 403)
	f.request("POST", endpoint, f.stranger, in, 404)
	raw, _ := f.request("POST", endpoint, f.writer, in, 200)
	reply := parse(t, raw)
	id := reply["snapshotId"].(string)
	if id == a.id || id == b.id || reply["rootDigest"] != a.root || reply["headAtCommit"] != id {
		t.Fatal(reply)
	}
	snap, _ := f.request("GET", f.path+"/snapshots/"+id, f.reader, nil, 200)
	point := parse(t, snap)
	if point["parentSnapshotId"] != b.id || point["restoredFromSnapshotId"] != a.id {
		t.Fatal(point)
	}
	for _, suffix := range []string{"/content?snapshotId=" + id + "&path=data.txt", "/snapshots/" + id + "/attachments/git.bundle"} {
		data, _ := f.request("GET", f.path+suffix, f.writer, nil, 200)
		if string(data) != "old body" {
			t.Fatal(string(data))
		}
	}
	f.del(a.id, id, 200)
	s, err := Open(f.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	web := httptest.NewTLSServer(s.Handler())
	defer web.Close()
	site := testSite{t, web}
	raw, _ = site.request("POST", endpoint, f.writer, in, 200)
	if parse(t, raw)["snapshotId"] != id {
		t.Fatal(string(raw))
	}
	in["label"] = "altered"
	site.request("POST", endpoint, f.writer, in, 409)
	in["requestId"] = newID()
	site.request("POST", endpoint, f.writer, in, 409)
	in["expectedHead"] = id
	site.request("POST", endpoint, f.writer, in, 404)
}

func TestTimelineDaysAndMergedForkRetention(t *testing.T) {
	f := timelineFixtureAt(t)
	url := f.path + "/history-policy"
	for _, v := range []historyPolicy{{"all", 1}, {"latest", 0}, {"latest", 10001}, {"days", 3651}, {"other", 1}} {
		f.request("PUT", url, f.writer, v, 400)
	}
	raw, _ := f.request("GET", url, f.reader, nil, 200)
	if parse(t, raw)["mode"] != "all" {
		t.Fatal(string(raw))
	}
	a := f.add("", "same", "published")
	b := f.add(a.id, "main", "published")
	fork := f.add(a.id, "branch", "fork")
	f.request("PUT", url, f.writer, historyPolicy{"days", 1}, 200)
	// capturedAt was 30 days ago; only the NAS commit clock counts.
	f.request("GET", f.path+"/snapshots/"+a.id, f.reader, nil, 200)
	if err := f.s.maintainRetention(context.Background(), time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.request("GET", f.path+"/snapshots/"+a.id, f.reader, nil, 404)
	f.request("GET", f.path+"/snapshots/"+b.id, f.reader, nil, 200)
	f.request("PUT", url, f.writer, historyPolicy{"latest", 1}, 200)
	raw, _ = f.request("POST", f.path+"/merge/commit", f.writer, fields{"mainHead": b.id, "branchSnapshotId": fork.id, "requestId": newID(), "label": "reconcile", "choices": []fields{{"path": "data.txt", "choice": "main"}}}, 200)
	id := parse(t, raw)["snapshotId"].(string)
	f.request("GET", f.path+"/snapshots/"+id, f.reader, nil, 200)
	f.request("GET", f.path+"/snapshots/"+fork.id, f.reader, nil, 404)
	f.request("POST", f.path+"/merge/plan", f.reader, fields{"branchSnapshotId": fork.id}, 404)
}

func TestTimelineConcurrencyAndPreview(t *testing.T) {
	f := timelineFixtureAt(t)
	a := f.add("", "a", "published")
	b := f.add(a.id, "b", "published")
	c := f.add(b.id, "c", "published")
	url := f.path + "/history-policy"
	p := historyPolicy{"latest", 2}
	raw, _ := f.request("POST", url+"/preview", f.writer, p, 200)
	prev := parse(t, raw)
	if prev["removeCount"] != float64(1) || prev["protectedCount"] != float64(1) {
		t.Fatal(prev)
	}
	f.request("GET", f.path+"/snapshots/"+a.id, f.reader, nil, 200)
	f.request("PUT", url, f.writer, fields{"mode": "latest", "value": 2, "expectedHead": b.id}, 409)
	// Source A and B compete to restore under one observed main head.
	type result struct {
		status int
		err    error
	}
	issue := func(method, path string, body any) result {
		data, err := json.Marshal(body)
		if err != nil {
			return result{0, err}
		}
		req, err := http.NewRequest(method, f.srv.URL+"/v1"+path, bytes.NewReader(data))
		if err != nil {
			return result{0, err}
		}
		req.Header.Set("Authorization", "Bearer "+f.writer)
		res, err := f.srv.Client().Do(req)
		if err != nil {
			return result{0, err}
		}
		_, err = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return result{res.StatusCode, err}
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, p := range []testPoint{a, b} {
		go func(id string) {
			<-start
			results <- issue("POST", f.path+"/timeline/restore", fields{"snapshotId": id, "expectedHead": c.id, "requestId": newID(), "label": "race"})
		}(p.id)
	}
	close(start)
	count := map[int]int{}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		count[r.status]++
	}
	if count[200] != 1 || count[409] != 1 {
		t.Fatal(count)
	}
	var head string
	if err := f.s.db.QueryRow("SELECT head FROM units WHERE id=?", f.unit).Scan(&head); err != nil {
		t.Fatal(err)
	}
	// Protection races a delete with a fresh preview. Exactly one can win.
	input := fields{"snapshotId": a.id, "expectedHead": head}
	raw, _ = f.request("POST", f.path+"/timeline/delete/preview", f.writer, input, 200)
	input["planToken"] = parse(t, raw)["planToken"]
	start = make(chan struct{})
	hold := f.path + "/holds/" + newID()
	go func() { <-start; results <- issue("PUT", hold, fields{"snapshotId": a.id, "path": "data.txt"}) }()
	go func() { <-start; results <- issue("POST", f.path+"/timeline/delete", input) }()
	close(start)
	count = map[int]int{}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		count[r.status]++
	}
	if count[200] != 1 || count[404]+count[409] != 1 {
		t.Fatal(count)
	}
	var active, hidden bool
	if err := f.s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM holds WHERE unit=? AND snapshot=? AND released=0),EXISTS(SELECT 1 FROM snapshot_tombstones WHERE unit=? AND snapshot=?)", f.unit, a.id, f.unit, a.id).Scan(&active, &hidden); err != nil || active == hidden {
		t.Fatal(active, hidden, err)
	}
}

func TestMigrateTimelineDataToCurrentSchema(t *testing.T) {
	for _, version := range []int{5, 6, 7} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"state", "objects", "incoming"} {
				if err := os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			marker := newID()
			if err := os.WriteFile(filepath.Join(dir, "store-id"), []byte(marker+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			db, err := sqlite(dir, true)
			if err != nil {
				t.Fatal(err)
			}
			oldSchema := strings.TrimSuffix(schema, schemaLibraryDelete)
			if version < 7 {
				oldSchema = strings.TrimSuffix(oldSchema, schemaConsole)
			}
			if version == 5 {
				oldSchema = strings.TrimSuffix(oldSchema, schemaTimeline)
			}
			if _, err = db.Exec(oldSchema); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("INSERT INTO meta VALUES(?)", marker); err != nil {
				t.Fatal(err)
			}
			var actual int
			if err = db.QueryRow("PRAGMA user_version").Scan(&actual); err != nil || actual != version {
				t.Fatal("unexpected source schema", actual, err)
			}
			unit, device, snap, token := newID(), newID(), newID(), secret()
			if _, err = db.Exec("INSERT INTO units VALUES(?,?,?)", unit, "existing", snap); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("INSERT INTO devices(id,name,verifier,maximum_role) VALUES(?,?,?,'sync')", device, "existing", hash([]byte(token))); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("INSERT INTO grants VALUES(?,?,'sync')", unit, device); err != nil {
				t.Fatal(err)
			}
			old := Store{dir: dir}
			root, err := objectfmt.BuildDir(nil, old.install)
			if err != nil {
				t.Fatal(err)
			}
			record, _ := json.Marshal(fields{"unitId": unit, "snapshotId": snap, "rootDigest": root, "outcome": "published", "links": []any{}, "attachments": []any{}, "committedAt": stamp(time.Now())})
			if _, err = db.Exec("INSERT INTO snapshots(unit,id,root,record) VALUES(?,?,?,?)", unit, snap, root, record); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("INSERT INTO refs VALUES(?,?,'tree',?)", unit, snap, root); err != nil {
				t.Fatal(err)
			}
			hold := newID()
			if _, err = db.Exec("INSERT INTO holds(id,unit,snapshot,path,device,created) VALUES(?,?,?,'',?,?)", hold, unit, snap, device, stamp(time.Now())); err != nil {
				t.Fatal(err)
			}
			if version == 6 {
				if _, err = db.Exec("INSERT INTO history_policies VALUES(?,'latest',4)", unit); err != nil {
					t.Fatal(err)
				}
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.db.QueryRow("PRAGMA user_version").Scan(&actual); err != nil || actual != 8 {
				t.Fatal(actual, err)
			}
			web := httptest.NewTLSServer(s.Handler())
			defer web.Close()
			site := testSite{t, web}
			p := "/units/" + unit
			body, _ := site.request("GET", p+"/snapshots/"+snap, token, nil, 200)
			if parse(t, body)["rootDigest"] != root {
				t.Fatal(string(body))
			}
			site.request("GET", p+"/snapshots/"+snap+"/objects/"+root+"?kind=tree", token, nil, 200)
			site.request("GET", p+"/holds/"+hold, token, nil, 200)
			body, _ = site.request("GET", p+"/history-policy", token, nil, 200)
			want := "all"
			if version == 6 {
				want = "latest"
			}
			if parse(t, body)["mode"] != want {
				t.Fatal(string(body))
			}
			var sessions int
			if err = s.db.QueryRow("SELECT count(*) FROM console_sessions").Scan(&sessions); err != nil || sessions != 0 {
				t.Fatal(sessions, err)
			}
		})
	}
}
