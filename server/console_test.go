package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

func TestConsoleSessionRolesAndLifecycle(t *testing.T) {
	f := timelineFixtureAt(t)
	a := f.add("", "example", "published")
	client := *f.srv.Client()
	client.Jar, _ = cookiejar.New(nil)
	call := func(method, path string, input any, origin string, want int) ([]byte, http.Header) {
		t.Helper()
		data, _ := json.Marshal(input)
		req, e := http.NewRequest(method, f.srv.URL+path, bytes.NewReader(data))
		if e != nil {
			t.Fatal(e)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, e := io.ReadAll(res.Body)
		if e != nil || res.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, res.StatusCode, want, b)
		}
		return b, res.Header
	}
	raw, _ := call("GET", "/console/session", nil, "", 200)
	if parse(t, raw)["paired"] != false {
		t.Fatal("unexpected session")
	}
	for _, role := range []string{"reader", "sync"} {
		code, e := f.s.Issue(f.unit, role)
		if e != nil {
			t.Fatal(e)
		}
		call("POST", "/console/pair", fields{"code": code}, "", 403)
		raw, h := call("POST", "/console/pair", fields{"code": code}, f.srv.URL, 201)
		cookies := (&http.Response{Header: h}).Cookies()
		if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
			t.Fatal("session attributes")
		}
		id := parse(t, raw)["deviceId"].(string)
		f.request("GET", "/units", cookies[0].Value, nil, 401)
		call("GET", "/v1/units", nil, "", 200)
		u := "/v1" + f.path
		httpCode := http.StatusForbidden
		if role == "sync" {
			httpCode = http.StatusOK
		}
		payload := historyPolicy{"all", 0}
		call("PUT", u+"/history-policy", payload, "", 403)
		call("PUT", u+"/history-policy", payload, f.srv.URL, httpCode)
		raw, h = call("GET", u+"/content?snapshotId="+a.id+"&path=data.txt", nil, "", 200)
		if string(raw) != "example" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
			t.Fatal("unsafe download")
		}
		raw, h = call("GET", u+"/archive?snapshotId="+a.id, nil, "", 200)
		if !bytes.HasPrefix(raw, []byte("PK\x03\x04")) || h.Get("Content-Type") != "application/zip" || !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
			t.Fatal("console ZIP download")
		}
		call("POST", "/console/logout", nil, f.srv.URL, 200)
		raw, _ = call("GET", "/console/session", nil, "", 200)
		if parse(t, raw)["paired"] != false {
			t.Fatal("logout")
		}
		var revoked bool
		if e = f.s.db.QueryRow("SELECT revoked FROM devices WHERE id=?", id).Scan(&revoked); e != nil || !revoked {
			t.Fatal("unrevoked", e)
		}
	}
}
