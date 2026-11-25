package server

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the same TLS, Origin and session cookie boundaries as a mobile browser.
func TestReaderBrowserSession(t *testing.T) {
	volume := t.TempDir()
	if err := Init(volume); err != nil {
		t.Fatal(err)
	}
	store, err := Open(volume)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	web := httptest.NewTLSServer(store.Handler())
	defer web.Close()
	client := web.Client()
	client.Jar, _ = cookiejar.New(nil)
	call := func(method, path, body string, want int, headers ...string) (string, http.Header) {
		t.Helper()
		req, err := http.NewRequest(method, web.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s got %d expected %d: %s", method, path, res.StatusCode, want, data)
		}
		return string(data), res.Header
	}
	body, h := call("GET", "/reader/", "", 200)
	if !strings.Contains(body, "心狐同步") || !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("reader shell or CSP missing")
	}
	for _, a := range []string{"reader.js", "reader.css", "xinhu.png"} {
		call("GET", "/reader/"+a, "", 200)
	}
	call("GET", "/reader/api/units", "", 401)
	src := t.TempDir()
	unit, err := store.AddExternal("公开文件", src)
	if err != nil {
		t.Fatal(err)
	}
	payload := "<h1>literal file</h1>"
	if err = os.WriteFile(filepath.Join(src, "example.html"), []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	other, err := store.AddUnit("secret")
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.Issue(unit, "reader")
	if err != nil {
		t.Fatal(err)
	}
	writerCode, err := store.Issue(other, "sync")
	if err != nil {
		t.Fatal(err)
	}
	pair := func(code string) string { b, _ := json.Marshal(fields{"code": code}); return string(b) }
	call("POST", "/reader/pair", pair(writerCode), 403, "Origin", web.URL)
	// A wrong role must not consume a CLI sync code.
	(testSite{t, web}).request("POST", "/pair", "", fields{"deviceId": newID(), "deviceName": "sync", "token": secret(), "code": writerCode}, 201)
	call("POST", "/reader/pair", pair(code), 403, "Origin", "https://other.invalid")
	_, h = call("POST", "/reader/pair", pair(code), 201, "Origin", web.URL)
	cookies := (&http.Response{Header: h}).Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode || !cookies[0].Expires.IsZero() || cookies[0].MaxAge != 0 {
		t.Fatal("cookie is not constrained to a browser session")
	}
	body, _ = call("GET", "/reader/api/units", "", 200)
	if !strings.Contains(body, unit) || strings.Contains(body, other) {
		t.Fatal(body)
	}
	call("GET", "/reader/api/units/"+other+"/entries", "", 404)
	call("GET", "/reader/api/units/"+unit+"/entries", "", 200)
	body, h = call("GET", "/reader/api/units/"+unit+"/content?path=example.html", "", 206, "Range", "bytes=0-3")
	kind, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if body != payload[:4] || err != nil || kind != "attachment" || params["filename"] != "example.html" || h.Get("Content-Type") != "application/octet-stream" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Fatal(body, h)
	}
	call("POST", "/reader/logout", "", 403, "Origin", "https://other.invalid")
	call("POST", "/reader/logout", "", 204, "Origin", web.URL)
	call("GET", "/reader/api/units", "", 401)
	call("GET", "/reader/api/units", "", 401, "Cookie", cookies[0].Name+"="+cookies[0].Value)
}
