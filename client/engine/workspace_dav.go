package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type WorkspaceVolumeReady struct {
	URL        string `json:"url"`
	MountPoint string `json:"mountPoint"`
}
type workspaceDAV struct {
	store                *Store
	remote               *Remote
	host, secret, device string
}
type davFile struct {
	slot, unit, snapshot, member, sha string
	size                              int64
}

// ServeWorkspaceVolume only reads rows that the desktop explicitly archived.
// The mount URL is OS-visible. Each slot is a separate, unlisted capability.
func (s *Store) ServeWorkspaceVolume(ctx context.Context, ready func(WorkspaceVolumeReady) error) error {
	var device string
	if err := s.db.QueryRowContext(ctx, "SELECT device FROM account WHERE id=1").Scan(&device); err != nil {
		return err
	}
	r, err := s.Remote(ctx)
	if err != nil {
		return err
	}
	defer r.client.CloseIdleConnections()
	guard, err := lockFile(filepath.Join(s.Home, "workspace-volume.lock"), true)
	if err != nil {
		return fmt.Errorf("工作区磁盘服务已在运行或无法加锁: %w", err)
	}
	defer guard.Close()
	mount := filepath.Join(s.Home, "workspace-volume")
	if err = os.Mkdir(mount, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return err
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	d := &workspaceDAV{store: s, remote: r, host: ln.Addr().String(), secret: base64.RawURLEncoding.EncodeToString(secret[:]), device: device}
	srv := &http.Server{Handler: d, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16384, BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { srv.Close() })
	defer stop()
	if err = ready(WorkspaceVolumeReady{URL: "http://" + d.host + "/" + d.secret + "/", MountPoint: mount}); err != nil {
		return err
	}
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	return err
}

func (d *workspaceDAV) lookup(ctx context.Context, slot string) (davFile, error) {
	var f davFile
	err := d.store.db.QueryRowContext(ctx, `SELECT w.slot,w.unit,w.snapshot,w.member,w.sha,w.size FROM workspace_archives w
		JOIN account a ON a.device=w.device AND a.id=1 WHERE w.slot=? AND w.device=? AND w.phase IN('releasing','online','hydrating')`, slot, d.device).Scan(&f.slot, &f.unit, &f.snapshot, &f.member, &f.sha, &f.size)
	if err != nil {
		return f, err
	}
	if validID(f.unit) != nil || validID(f.snapshot) != nil || !remotePathOK(f.member) || f.member == "" || f.size < 0 || len(f.sha) != 64 {
		return f, errors.New("invalid archived record")
	}
	if _, err := hex.DecodeString(f.sha); err != nil {
		return f, err
	}
	return f, nil
}
func (d *workspaceDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if r.Host != d.host || r.URL.RawQuery != "" || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Mode") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		http.NotFound(w, r)
		return
	}
	// Never normalize traversal, encoded separators, or aliases for a capability.
	if strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2f") || !utf8.ValidString(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	urlPath := strings.TrimPrefix(r.URL.Path, "/")
	key, rest, _ := strings.Cut(urlPath, "/")
	if subtle.ConstantTimeCompare([]byte(key), []byte(d.secret)) != 1 {
		http.NotFound(w, r)
		return
	}
	segments := []string{}
	if rest != "" {
		segments = strings.Split(strings.TrimSuffix(rest, "/"), "/")
	}
	var f davFile
	if len(segments) > 0 {
		slot := segments[0]
		if len(segments) > 2 || len(slot) > 128 || len(slot) < 16 || strings.ContainsAny(slot, "/\\\x00.") {
			http.NotFound(w, r)
			return
		}
		var err error
		f, err = d.lookup(r.Context(), slot)
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "工作区暂时不可用", 503)
			return
		}
		if len(segments) == 2 && (segments[1] != path.Base(f.member) || strings.HasSuffix(rest, "/")) {
			http.NotFound(w, r)
			return
		}
	}
	dir := len(segments) < 2
	w.Header().Set("Allow", "OPTIONS, PROPFIND, GET, HEAD")
	w.Header().Set("DAV", "1")
	switch r.Method {
	case "OPTIONS":
		w.WriteHeader(http.StatusOK)
	case "PROPFIND":
		d.properties(w, r, f, dir, len(segments) == 0)
	case "HEAD", "GET":
		if dir {
			w.WriteHeader(405)
			return
		}
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", strconv.FormatInt(f.size, 10))
			w.Header().Set("ETag", strconv.Quote(f.sha))
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Type", "application/octet-stream")
			return
		}
		d.content(w, r, f)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (d *workspaceDAV) content(w http.ResponseWriter, r *http.Request, f davFile) {
	q := url.Values{"snapshotId": {f.snapshot}, "path": {f.member}}
	req, err := http.NewRequestWithContext(r.Context(), "GET", d.remote.base+"/units/"+f.unit+"/content?"+q.Encode(), nil)
	if err != nil {
		http.Error(w, "读取失败", 502)
		return
	}
	req.Header.Set("Authorization", "Bearer "+d.remote.token)
	req.Header.Set("If-Match", strconv.Quote(f.sha))
	requestRange := r.Header.Get("Range")
	if requestRange != "" {
		req.Header.Set("Range", requestRange)
	}
	up, err := d.remote.client.Do(req)
	if err != nil {
		http.Error(w, "暂时无法连接 NAS", 503)
		return
	}
	defer up.Body.Close()
	if up.StatusCode == 416 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", f.size))
		w.WriteHeader(416)
		return
	}
	if up.StatusCode == 404 || up.StatusCode == 403 || up.StatusCode == 401 {
		http.NotFound(w, r)
		return
	}
	if up.Header.Get("ETag") != strconv.Quote(f.sha) {
		http.Error(w, "归档版本校验失败", 502)
		return
	}
	length := up.ContentLength
	if requestRange == "" {
		if up.StatusCode != 200 || length != f.size {
			http.Error(w, "归档长度不符", 502)
			return
		}
	} else {
		start, end, ok := davRange(requestRange, f.size)
		expected := fmt.Sprintf("bytes %d-%d/%d", start, end, f.size)
		if !ok || up.StatusCode != 206 || length != end-start+1 || up.Header.Get("Content-Range") != expected {
			http.Error(w, "归档字节范围不符", 502)
			return
		}
		w.Header().Set("Content-Range", expected)
	}
	w.Header().Set("ETag", strconv.Quote(f.sha))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(up.StatusCode)
	if length == 0 {
		return
	}
	if length != f.size {
		// The NAS verifies each chunk it reads. The strong ETag and precise
		// Content-Range above tie this partial response to the pinned file.
		if _, err = io.CopyN(w, up.Body, length); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	// Do not deliver a complete response to an opener if the digest disagrees.
	// Holding back one byte makes a broken transfer observable to WebDAVFS.
	h := sha256.New()
	if _, err = io.CopyN(io.MultiWriter(w, h), up.Body, length-1); err != nil {
		panic(http.ErrAbortHandler)
	}
	var last [1]byte
	if _, err = io.ReadFull(up.Body, last[:]); err != nil {
		panic(http.ErrAbortHandler)
	}
	h.Write(last[:])
	if hex.EncodeToString(h.Sum(nil)) != f.sha {
		panic(http.ErrAbortHandler)
	}
	_, _ = w.Write(last[:])
}
func davRange(header string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || size <= 0 {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	parse := func(s string) (int64, bool) {
		if s == "" {
			return 0, false
		}
		for _, v := range s {
			if v < '0' || v > '9' {
				return 0, false
			}
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	if a == "" {
		n, ok := parse(b)
		if !ok || n <= 0 {
			return 0, 0, false
		}
		return max(0, size-n), size - 1, true
	}
	start, ok := parse(a)
	if !ok || start >= size {
		return 0, 0, false
	}
	if b == "" {
		return start, size - 1, true
	}
	end, ok := parse(b)
	if !ok || end < start {
		return 0, 0, false
	}
	return start, min(end, size-1), true
}
