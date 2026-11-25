package server

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestDeleteLibraryScopeAndSharedObjects(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	web := httptest.NewTLSServer(s.Handler())
	defer web.Close()
	site := testSite{t, web}
	add := func(name string) string {
		t.Helper()
		id, e := s.AddUnit(name)
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	scope := add("测试空间")
	other := add("留存资料库")
	writer, dev := site.enroll(s, scope, "sync")
	reader, _ := site.enroll(s, scope, "reader")
	base := "/units/" + scope
	site.request("GET", base+"/delete-plan", writer, nil, 403)
	if err = s.setDeleteManager(scope, dev, true); err != nil {
		t.Fatal(err)
	}
	site.request("GET", base+"/delete-plan", reader, nil, 403)
	getPlan := func() fields { b, _ := site.request("GET", base+"/delete-plan", writer, nil, 200); return parse(t, b) }
	original := getPlan()
	data, _ := site.request("POST", base+"/projects", writer, fields{"key": "sample", "name": "样本"}, 201)
	child := parse(t, data)["unitId"].(string)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := s.db.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	shared := objectfmt.Ref{Kind: "chunk", Digest: objectfmt.Digest([]byte("shared content"))}
	exclusive := objectfmt.Ref{Kind: "chunk", Digest: objectfmt.Digest([]byte("only here"))}
	if err = s.install(shared, []byte("shared content")); err != nil {
		t.Fatal(err)
	}
	if err = s.install(exclusive, []byte("only here")); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{other, child} {
		id := newID()
		exec("INSERT INTO snapshots(unit,id,root,record) VALUES(?,?,?,'{}')", u, id, shared.Digest)
		exec("INSERT INTO refs VALUES(?,?,?,?)", u, id, shared.Kind, shared.Digest)
		if u == child {
			exec("INSERT INTO refs VALUES(?,?,?,?)", u, id, exclusive.Kind, exclusive.Digest)
		}
	}
	site.request("POST", base+"/delete", writer, fields{"name": "测试空间", "token": original["token"]}, 409)
	valid := getPlan()
	if valid["units"] != float64(2) || valid["snapshots"] != float64(1) || valid["devices"] != float64(2) {
		t.Fatal(valid)
	}
	site.request("POST", base+"/delete", writer, fields{"name": "不同", "token": valid["token"]}, 409)
	site.request("POST", base+"/delete", writer, fields{"name": "测试空间", "token": valid["token"]}, 200)
	site.request("GET", base+"/delete-plan", writer, nil, 404)
	if _, err = s.collectLibraryObjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(s.path(shared)); err != nil || string(b) != "shared content" {
		t.Fatal("shared object lost", err)
	}
	if _, err = os.Stat(s.path(exclusive)); !os.IsNotExist(err) {
		t.Fatal("orphan not collected", err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM units WHERE id=?", other).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
