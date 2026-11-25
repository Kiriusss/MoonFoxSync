package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"testing"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestSnapshotArchive(t *testing.T) {
	f := timelineFixtureAt(t)
	first := f.add("", "first revision", "published")
	second := f.add(first.id, "second revision", "published")
	if _, err := f.s.db.Exec("UPDATE units SET name=? WHERE id=?", "心狐/源代码", f.unit); err != nil {
		t.Fatal(err)
	}
	endpoint := f.path + "/archive?snapshotId=" + first.id
	code, err := f.s.Issue(f.unit, "reader")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(fields{"code": code})
	pair, _ := http.NewRequest("POST", f.srv.URL+"/reader/pair", bytes.NewReader(payload))
	pair.Header.Set("Origin", f.srv.URL)
	paired, err := f.srv.Client().Do(pair)
	if err != nil {
		t.Fatal(err)
	}
	paired.Body.Close()
	if paired.StatusCode != 201 || len(paired.Cookies()) != 1 {
		t.Fatal("reader pairing failed")
	}
	readerDownload, _ := http.NewRequest("GET", f.srv.URL+"/reader/api"+endpoint, nil)
	readerDownload.AddCookie(paired.Cookies()[0])
	res, err := f.srv.Client().Do(readerDownload)
	if err != nil {
		t.Fatal(err)
	}
	readerZIP, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 {
		t.Fatal("reader ZIP download", err, res.StatusCode)
	}
	f.request("GET", endpoint, "", nil, 401)
	f.request("GET", endpoint, f.stranger, nil, 404)
	f.request("GET", f.path+"/archive", f.reader, nil, 400)
	f.request("GET", f.path+"/archive?snapshotId="+newID(), f.reader, nil, 404)
	body, h := f.request("GET", endpoint, f.reader, nil, 200, "Range", "bytes=1-3")
	if !bytes.Equal(body, readerZIP) {
		t.Fatal("reader download did not match selected snapshot")
	}
	disposition, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || !strings.HasPrefix(params["filename"], "心狐%2F源代码-") || !strings.HasSuffix(params["filename"], ".zip") || h.Get("Accept-Ranges") != "none" || h.Get("Content-Type") != "application/zip" {
		t.Fatal("archive headers", h)
	}
	z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.TrimSuffix(params["filename"], ".zip")
	b, err := fs.ReadFile(z, prefix+"/data.txt")
	if err != nil || string(b) != "first revision" || len(z.File) != 2 {
		t.Fatal("unexpected download", string(b), err)
	}
	body, _ = f.request("HEAD", endpoint, f.reader, nil, 200)
	if len(body) != 0 {
		t.Fatal("HEAD response body")
	}
	for i := 0; i < cap(f.s.archiveSlots); i++ {
		f.s.archiveSlots <- struct{}{}
	}
	f.request("GET", endpoint, f.reader, nil, 429)
	for i := 0; i < cap(f.s.archiveSlots); i++ {
		<-f.s.archiveSlots
	}
	f.del(first.id, second.id, 200)
	f.request("GET", endpoint, f.reader, nil, 404)
}

func TestArchiveNamesAndLinkTargets(t *testing.T) {
	mem := map[objectfmt.Ref][]byte{}
	save := func(r objectfmt.Ref, b []byte) error { mem[r] = bytes.Clone(b); return nil }
	load := func(r objectfmt.Ref) ([]byte, error) { return mem[r], nil }
	emptyDir, err := objectfmt.BuildDir(nil, save)
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Repeat("original bytes", 10000)
	file, err := objectfmt.BuildFile(strings.NewReader(data), save)
	if err != nil {
		t.Fatal(err)
	}
	entries := []objectfmt.Entry{
		{Name: "..\\outside", Kind: "symlink", Target: "/outside/path"},
		{Name: "..%5Coutside", Kind: "file", Digest: file, Executable: true},
		{Name: "空目录", Kind: "directory", Digest: emptyDir},
		{Name: "CON.txt", Kind: "file", Digest: file},
		{Name: "..:stream", Kind: "file", Digest: file},
	}
	var dst bytes.Buffer
	z := zip.NewWriter(&dst)
	if err := (archiveStream{out: z, load: load}).directory(context.Background(), "root/", entries); err != nil {
		t.Fatal(err)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := zip.NewReader(bytes.NewReader(dst.Bytes()), int64(dst.Len()))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"..%5Coutside": "/outside/path", "..%255Coutside": data, "%43ON.txt": data, "..%3Astream": data}
	for _, f := range got.File {
		if f.Mode()&os.ModeSymlink != 0 || !strings.HasPrefix(f.Name, "root/") || strings.Contains(f.Name, "\\") || strings.Contains(f.Name, "../") {
			t.Fatal("unsafe ZIP entry", f.Name, f.Mode())
		}
		if f.Mode().IsDir() {
			if f.Name != "root/空目录/" {
				t.Fatal(f.Name)
			}
			continue
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(r)
		r.Close()
		name := path.Base(f.Name)
		if err != nil || string(content) != want[name] {
			t.Fatal(name, "contents differ", err)
		}
		delete(want, name)
		if name == "..%255Coutside" && f.Mode().Perm() != 0755 {
			t.Fatal("lost executable bit")
		}
	}
	if len(want) != 0 || len(got.File) != len(entries) {
		t.Fatal("missing entries", want)
	}
}

func TestArchivePreservesDirectoryNamesAndCase(t *testing.T) {
	mem := map[objectfmt.Ref][]byte{}
	save := func(r objectfmt.Ref, b []byte) error { mem[r] = bytes.Clone(b); return nil }
	load := func(r objectfmt.Ref) ([]byte, error) { return mem[r], nil }
	file, err := objectfmt.BuildFile(strings.NewReader("shared subtree"), save)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := objectfmt.BuildDir([]objectfmt.Entry{{Kind: "file", Name: "ReadMe", Digest: file}, {Kind: "file", Name: "README", Digest: file}}, save)
	if err != nil {
		t.Fatal(err)
	}
	long1 := strings.Repeat("%", 240) + "ABC"
	long2 := strings.Repeat("%", 240) + "abc"
	entries := []objectfmt.Entry{{Kind: "directory", Name: "Source", Digest: dir}, {Kind: "directory", Name: "source", Digest: dir}, {Kind: "file", Name: long1, Digest: file}, {Kind: "file", Name: long2, Digest: file}}
	var dst bytes.Buffer
	out := zip.NewWriter(&dst)
	if err := (archiveStream{out: out, load: load}).directory(context.Background(), "", entries); err != nil {
		t.Fatal(err)
	}
	if err = out.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := zip.NewReader(bytes.NewReader(dst.Bytes()), int64(dst.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Source/ReadMe", "Source/README", "source/ReadMe", "source/README"} {
		content, err := fs.ReadFile(got, p)
		if err != nil || string(content) != "shared subtree" {
			t.Fatal(p, err)
		}
	}
	n := len(got.File)
	if n != 8 || got.File[n-1].Name == got.File[n-2].Name || len(got.File[n-1].Name) > 255 || got.File[n-1].Comment != "Original name: "+long2 {
		t.Fatal("long names lost")
	}
}

type canceledDownload struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w canceledDownload) Flush() {
	w.ResponseRecorder.Flush()
	w.cancel()
}

func TestArchiveDisconnectReleasesResources(t *testing.T) {
	f := timelineFixtureAt(t)
	p := f.add("", "some content", "published")
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, "GET", "/v1"+f.path+"/archive?snapshotId="+p.id, nil)
	req.SetPathValue("unitId", f.unit)
	w := canceledDownload{httptest.NewRecorder(), cancel}
	var device string
	if err := f.s.db.QueryRow("SELECT id FROM devices WHERE verifier=?", hash([]byte(f.reader))).Scan(&device); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Fatal("expected truncated stream to abort")
			}
		}()
		_ = f.s.archive(w, req, device)
	}()
	if len(f.s.archiveSlots) != 0 || !f.s.archiveUse.TryLock() {
		t.Fatal("download resources leaked")
	}
	f.s.archiveUse.Unlock()
	if _, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len())); err == nil {
		t.Fatal("partial archive looks complete")
	}
}
