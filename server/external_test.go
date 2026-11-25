//go:build darwin || linux

package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExternalPrivateRoot(t *testing.T) {
	space := t.TempDir()
	data := filepath.Join(space, "db")
	source := filepath.Join(space, "docs")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if e := Init(data); e != nil {
		t.Fatal(e)
	}
	s, e := Open(data)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.AddExternal("secret", space); e == nil {
		t.Fatal("data directory would be exposed")
	}
	unit, e := s.AddExternal("资料", source)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(source, "a.txt")
	if e = os.WriteFile(p, []byte("abc"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(filepath.Join(data, "store-id"), filepath.Join(source, "key")); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(data, filepath.Join(source, "alias")); e != nil {
		t.Fatal(e)
	}
	h := httptest.NewTLSServer(s.Handler())
	defer h.Close()
	site := testSite{t, h}
	token, _ := site.enroll(s, unit, "reader")
	base := "/units/" + unit
	list, _ := site.request("GET", "/units", token, nil, 200)
	var units struct{ Items []struct{ Kind string } }
	if e = json.Unmarshal(list, &units); e != nil || len(units.Items) != 1 || units.Items[0].Kind != "external" {
		t.Fatalf("units %s: %v", list, e)
	}
	first, _ := site.request("GET", base+"/entries?path=&limit=1", token, nil, 200)
	cursor := parse(t, first)["nextCursor"].(string)
	// Ensure directory ctime is different even on a coarse filesystem.
	time.Sleep(2 * time.Millisecond)
	if e = os.WriteFile(filepath.Join(source, "b.txt"), []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	site.request("GET", base+"/entries?path=&limit=1&cursor="+cursor, token, nil, 409)
	path := base + "/content?path=a.txt"
	b, headers := site.request("GET", path, token, nil, 206, "Range", "bytes=1-1")
	if string(b) != "b" {
		t.Fatal(string(b))
	}
	if e = os.WriteFile(p, []byte("xyz"), 0600); e != nil {
		t.Fatal(e)
	}
	site.request("GET", path, token, nil, 412, "Range", "bytes=2-2", "If-Match", headers.Get("ETag"))
	b, _ = site.request("GET", path, token, nil, 200)
	if string(b) != "xyz" {
		t.Fatal(string(b))
	}
	site.request("GET", base+"/content?path=key", token, nil, 404)
	site.request("GET", base+"/content?path=alias/store-id", token, nil, 404)
	site.request("GET", base+"/content?path=%2e%2e/db/store-id", token, nil, 400)
	site.request("POST", base+"/uploads", token, fields{}, 403)
	site.request("GET", base+"/entries?path=&snapshotId="+newID(), token, nil, 400)
	if e = os.Rename(source, source+".old"); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(source, 0700); e != nil {
		t.Fatal(e)
	}
	site.request("GET", base+"/entries?path=", token, nil, 409)
}
func TestCursorChangeFeed(t *testing.T) {
	dir := t.TempDir()
	if e := Init(dir); e != nil {
		t.Fatal(e)
	}
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	u, e := s.AddUnit("one")
	if e != nil {
		t.Fatal(e)
	}
	h := httptest.NewTLSServer(s.Handler())
	defer h.Close()
	site := testSite{t, h}
	token, id := site.enroll(s, u, "sync")
	initial, _ := site.request("GET", "/units", token, nil, 200)
	cursor := parse(t, initial)["syncCursor"].(string)
	oldHead := ""
	var firstPage string
	for i := 0; i < 3; i++ {
		sid := newID()
		_, e = s.db.Exec("INSERT INTO snapshots(unit,id,root,record) VALUES(?,?,?,?)", u, sid, hash(nil), fmt.Sprintf(`{"outcome":"published","snapshotId":"%s"}`, sid))
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			oldHead = sid
		}
		if i == 1 {
			page, _ := site.request("GET", "/units/"+u+"/snapshots?limit=1", token, nil, 200)
			firstPage = parse(t, page)["nextCursor"].(string)
		}
	}
	p, _ := site.request("GET", "/units/"+u+"/snapshots?limit=1&cursor="+firstPage, token, nil, 200)
	if parse(t, p)["items"].([]any)[0].(map[string]any)["snapshotId"] != oldHead {
		t.Fatalf("unstable page %s", p)
	}
	next, _ := site.request("GET", "/changes?limit=1&cursor="+cursor, token, nil, 200)
	if !parse(t, next)["hasMore"].(bool) {
		t.Fatal(string(next))
	}
	other, e := s.AddUnit("new grant")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Grant(other, id, "reader"); e != nil {
		t.Fatal(e)
	}
	site.request("GET", "/changes?cursor="+cursor, token, nil, 410)
}
