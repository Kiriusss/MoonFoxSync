package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testSite struct {
	t   *testing.T
	srv *httptest.Server
}

func (h testSite) request(method, path, token string, body any, status int, headers ...string) ([]byte, http.Header) {
	h.t.Helper()
	var data []byte
	if body != nil {
		var e error
		data, e = json.Marshal(body)
		if e != nil {
			h.t.Fatal(e)
		}
	}
	req, e := http.NewRequest(method, h.srv.URL+"/v1"+path, bytes.NewReader(data))
	if e != nil {
		h.t.Fatal(e)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, e := h.srv.Client().Do(req)
	if e != nil {
		h.t.Fatal(e)
	}
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil {
		h.t.Fatal(e)
	}
	if res.StatusCode != status {
		h.t.Fatalf("%s %s want %d got %d: %s", method, path, status, res.StatusCode, b)
	}
	return b, res.Header
}
func (h testSite) enroll(store *Store, u, role string) (token, id string) {
	h.t.Helper()
	code, e := store.Issue(u, role)
	if e != nil {
		h.t.Fatal(e)
	}
	token = secret()
	id = newID()
	req := fields{"deviceId": id, "deviceName": "test", "code": code, "token": token}
	h.request("POST", "/pair", "", req, 201)
	h.request("POST", "/pair", "", req, 401)
	return
}
func parse(t *testing.T, b []byte) fields {
	t.Helper()
	var v fields
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func TestManagedSnapshotLifecycle(t *testing.T) {
	dir := t.TempDir()
	if e := Init(dir); e != nil {
		t.Fatal(e)
	}
	store, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	web := httptest.NewTLSServer(store.Handler())
	site := testSite{t, web}
	defer func() { web.Close(); store.Close() }()
	u, e := store.AddUnit("first")
	if e != nil {
		t.Fatal(e)
	}
	other, e := store.AddUnit("private")
	if e != nil {
		t.Fatal(e)
	}
	writer, _ := site.enroll(store, u, "sync")
	reader, rid := site.enroll(store, u, "reader")
	stranger, _ := site.enroll(store, other, "sync")
	site.request("GET", "/units", "", nil, 401)
	data, e := os.ReadFile(filepath.Join("..", "docs", "implementation", "fixtures", "first-sync.json"))
	if e != nil {
		t.Fatal(e)
	}
	var fixture struct {
		Steps []struct {
			OperationID string
			Request     json.RawMessage
		}
	}
	if e = json.Unmarshal(data, &fixture); e != nil {
		t.Fatal(e)
	}
	unit := "/units/" + u
	draft := parse(t, fixture.Steps[0].Request)
	snap := draft["clientSnapshotId"].(string)
	site.request("POST", unit+"/uploads", reader, draft, 403)
	site.request("POST", unit+"/uploads", stranger, draft, 404)
	begin := func(d fields) string {
		v, _ := site.request("POST", unit+"/uploads", writer, d, 201)
		return "/uploads/" + parse(t, v)["uploadId"].(string)
	}
	session := begin(draft)
	site.request("POST", session+"/missing", writer, fixture.Steps[1].Request, 200)
	fail, _ := site.request("POST", session+"/finalize", writer, nil, 409)
	if parse(t, fail)["code"] != "OBJECTS_MISSING" {
		t.Fatal(string(fail))
	}
	site.request("GET", unit+"/snapshots/"+snap, reader, nil, 404)
	// A damaged member must not undo its valid neighbors. Retrying the
	// negotiation after a partial batch asks only for unconfirmed references.
	valid := fields{"kind": "chunk", "digest": hash([]byte("abc")), "data": []byte("abc")}
	invalid := fields{"kind": "chunk", "digest": valid["digest"], "data": []byte("xyz")}
	partial, _ := site.request("POST", session+"/batch", writer, fields{"items": []fields{invalid, valid, valid}}, 200)
	var statuses struct{ Items []struct{ Status string } }
	if err := json.Unmarshal(partial, &statuses); err != nil || len(statuses.Items) != 3 || statuses.Items[0].Status != "invalid" || statuses.Items[1].Status != "stored" || statuses.Items[2].Status != "exists" {
		t.Fatalf("mixed batch statuses: %s, %v", partial, err)
	}
	remaining, _ := site.request("POST", session+"/missing", writer, fixture.Steps[1].Request, 200)
	if bytes.Contains(remaining, []byte(hash([]byte("abc")))) {
		t.Fatalf("confirmed chunk was requested again: %s", remaining)
	}
	site.request("POST", session+"/batch", writer, fixture.Steps[3].Request, 200)
	first, _ := site.request("POST", session+"/finalize", writer, nil, 200)
	if parse(t, first)["outcome"] != "published" {
		t.Fatal(string(first))
	}
	again, _ := site.request("POST", session+"/finalize", writer, nil, 200)
	if !bytes.Equal(first, again) {
		t.Fatal("commit was not idempotent")
	}
	listed, _ := site.request("GET", unit+"/entries?snapshotId="+snap+"&path=", reader, nil, 200)
	if !strings.Contains(string(listed), "a.txt") || !strings.Contains(string(listed), "empty") {
		t.Fatal(string(listed))
	}
	path := unit + "/content?snapshotId=" + snap + "&path=a.txt"
	b, h := site.request("GET", path, reader, nil, 206, "Range", "bytes=1-1")
	if string(b) != "b" || h.Get("Content-Range") != "bytes 1-1/3" || h.Get("ETag") != fmt.Sprintf("%q", hash([]byte("abc"))) {
		t.Fatal(string(b), h)
	}
	site.request("GET", path, reader, nil, 412, "If-Match", `"changed"`)
	site.request("GET", path, reader, nil, 416, "Range", "bytes=5-6")
	fork := newID()
	draft["clientSnapshotId"] = fork
	draft["requestId"] = newID()
	f := begin(draft)
	b, _ = site.request("POST", f+"/finalize", writer, nil, 200)
	if parse(t, b)["outcome"] != "fork" || parse(t, b)["headAtCommit"] != snap {
		t.Fatal(string(b))
	}
	diff, _ := site.request("GET", unit+"/diff?from="+snap+"&to="+fork, reader, nil, 200)
	if len(parse(t, diff)["items"].([]any)) != 0 {
		t.Fatalf("identical forks differ: %s", diff)
	}
	site.request("GET", unit+"/diff?from="+snap+"&to="+fork, stranger, nil, 404)
	hpath := unit + "/holds/" + newID()
	hv := fields{"snapshotId": snap, "path": "a.txt"}
	site.request("PUT", hpath, writer, hv, 200)
	site.request("PUT", hpath, reader, hv, 403)
	// Persisted objects, ACLs, holds and both versions must survive a NAS restart.
	web.Close()
	store.Close()
	store, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	web = httptest.NewTLSServer(store.Handler())
	site.srv = web
	b, _ = site.request("GET", path, reader, nil, 200)
	if string(b) != "abc" {
		t.Fatal(string(b))
	}
	site.request("GET", hpath, writer, nil, 200)
	site.request("DELETE", hpath, writer, nil, 204)
	site.request("PUT", hpath, writer, hv, 409)
	if e = store.Revoke(rid); e != nil {
		t.Fatal(e)
	}
	site.request("GET", path, reader, nil, 401)
}
