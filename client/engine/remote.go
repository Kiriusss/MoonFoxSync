package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

type Remote struct {
	store       *Store
	base, token string
	stateHome   string
	peerPin     string
	recordPeer  bool
	client      *http.Client
}

// Commit requests can trigger a full verification of surviving snapshots before
// the NAS sends its first header. Only this innermost transport changes waiting
// behavior. The network-route check and traffic meter still see every request.
type commitWaitKey struct{}

type commitTransport struct {
	normal *http.Transport
	once   sync.Once
	slow   *http.Transport
}

func (t *commitTransport) longWait() *http.Transport {
	t.once.Do(func() {
		t.slow = t.normal.Clone()
		t.slow.ResponseHeaderTimeout = 0
	})
	return t.slow
}
func (t *commitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Context().Value(commitWaitKey{}) == true {
		return t.longWait().RoundTrip(req)
	}
	return t.normal.RoundTrip(req)
}
func (t *commitTransport) CloseIdleConnections() {
	t.normal.CloseIdleConnections()
	t.longWait().CloseIdleConnections()
}
func (r *Remote) commitCall(ctx context.Context, method, path string, input, output any) error {
	return r.call(context.WithValue(ctx, commitWaitKey{}, true), method, path, nil, input, output)
}

type unitInfo struct {
	ScopeID string  `json:"scopeId"`
	ID      string  `json:"unitId"`
	Kind    string  `json:"kind"`
	Role    string  `json:"role"`
	Head    *string `json:"head"`
}
type Page struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor *string           `json:"nextCursor"`
}

func makeRemote(address string, ca []byte, token string) (*Remote, error) {
	u, e := url.Parse(address)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/" && u.Path != "/v1") {
		return nil, errors.New("需要可信的 HTTPS 服务地址，例如 https://nas:7443")
	}
	var pool *x509.CertPool
	if len(ca) > 0 {
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("无效的 PEM CA 证书")
		}
	}
	t := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 2 * time.Minute, IdleConnTimeout: 60 * time.Second, MaxIdleConnsPerHost: maxFileTransfers}
	return &Remote{base: "https://" + u.Host + "/v1", token: token, client: &http.Client{Transport: &commitTransport{normal: t}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func readSecret(device string) (string, error) {
	if path := os.Getenv("KDRIVE_TOKEN_FILE"); path != "" {
		info, e := os.Lstat(path)
		if e != nil {
			return "", e
		}
		if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
			return "", errors.New("KDRIVE_TOKEN_FILE 必须是仅当前用户可读的普通文件")
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return "", e
		}
		return strings.TrimSpace(string(b)), nil
	}
	return keyring.Get("xinhu-sync", device)
}
func writeSecret(device, token string) error {
	if path := os.Getenv("KDRIVE_TOKEN_FILE"); path != "" {
		if runtime.GOOS == "windows" {
			return errors.New("Windows 设备凭据须存入凭据管理器，请移除 KDRIVE_TOKEN_FILE")
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = io.WriteString(f, token+"\n")
		if e == nil {
			e = f.Sync()
		}
		ec := f.Close()
		if e == nil {
			e = ec
		}
		if e != nil {
			return e
		}
		return syncDirectory(filepath.Dir(path))
	}
	return keyring.Set("xinhu-sync", device, token)
}

// LocalStatus contains the saved endpoint, even when the NAS is offline.
type LocalStatus struct {
	Paired    bool   `json:"paired"`
	ServerURL string `json:"serverUrl,omitempty"`
}

func (s *Store) LocalStatus(ctx context.Context) (LocalStatus, error) {
	var status LocalStatus
	err := s.db.QueryRowContext(ctx, "SELECT url, EXISTS(SELECT 1 FROM service_identity WHERE id=1) FROM account WHERE id=1").Scan(&status.ServerURL, &status.Paired)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	return status, err
}

func (s *Store) Remote(ctx context.Context) (*Remote, error) {
	var addr, device string
	var ca []byte
	e := s.db.QueryRowContext(ctx, "SELECT url,ca,device FROM account WHERE id=1").Scan(&addr, &ca, &device)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, errors.New("客户端尚未配对")
	}
	if e != nil {
		return nil, e
	}
	tok, e := readSecret(device)
	if e != nil {
		return nil, fmt.Errorf("读取设备凭据: %w", e)
	}
	r, err := makeRemote(addr, ca, tok)
	if err == nil {
		r.stateHome = s.Home
		r.store = s
	}
	return r, err
}
func (s *Store) Pair(ctx context.Context, address, name, code string, ca []byte) (map[string]any, error) {
	activity, lockErr := s.localLock(false)
	if lockErr != nil {
		return nil, lockErr
	}
	defer activity.Close()
	if code == "" {
		return nil, errors.New("请输入配对码")
	}
	r, e := makeRemote(address, ca, "")
	if e != nil {
		return nil, e
	}
	r.recordPeer = true
	addr := strings.TrimSuffix(r.base, "/v1")
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var oldURL, device string
	var oldCA []byte
	e = tx.QueryRowContext(ctx, "SELECT url,device,ca FROM account WHERE id=1").Scan(&oldURL, &device, &oldCA)
	if errors.Is(e, sql.ErrNoRows) {
		device = newID()
		var b [32]byte
		rand.Read(b[:])
		token := base64.RawURLEncoding.EncodeToString(b[:])
		if _, e = tx.ExecContext(ctx, "INSERT INTO account VALUES(1,?,?,?)", addr, ca, device); e != nil {
			return nil, e
		}
		if filename := os.Getenv("KDRIVE_TOKEN_FILE"); filename != "" {
			filename, err := filepath.Abs(filename)
			if err != nil {
				return nil, err
			}
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(token+"\n")))
			if _, e = tx.ExecContext(ctx, "INSERT INTO credential_file VALUES(?,?,?)", device, filename, digest); e != nil {
				return nil, e
			}
		}
		if e = writeSecret(device, token); e != nil {
			return nil, e
		}
	} else if e != nil {
		return nil, e
	} else if oldURL != addr || !bytes.Equal(oldCA, ca) {
		return nil, errors.New("请为不同服务使用独立的 KDRIVE_HOME 和设备凭据")
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	token, e := readSecret(device)
	if e != nil {
		return nil, e
	}
	var out map[string]any
	e = r.call(ctx, "POST", "/pair", nil, map[string]string{"code": code, "deviceId": device, "deviceName": name, "token": token}, &out)
	if e != nil {
		r.token = token
		// A response can be lost after the server consumes its single-use code.
		var probe Page
		if p := r.call(ctx, "GET", "/units", url.Values{"limit": {"1"}}, nil, &probe); p != nil {
			return nil, e
		}
		out = map[string]any{"deviceId": device, "paired": true}
	}
	_, e = s.db.ExecContext(ctx, "INSERT INTO service_identity VALUES(1,?) ON CONFLICT(id) DO UPDATE SET pin=excluded.pin", r.peerPin)
	return out, e
}

type APIError struct {
	HTTP                  int `json:"-"`
	Code, Message, Action string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("梦枢天罗 HTTP %d %s: %s", e.HTTP, e.Code, e.Message)
}
func (r *Remote) request(ctx context.Context, method, path string, q url.Values, body []byte, typ string) (*http.Response, error) {
	return r.requestObserved(ctx, method, path, q, body, typ, nil)
}

func (r *Remote) requestObserved(ctx context.Context, method, path string, q url.Values, body []byte, typ string, observe bodyObserver) (*http.Response, error) {
	addr := r.base + path
	if len(q) > 0 {
		addr += "?" + q.Encode()
	}
	req, e := http.NewRequestWithContext(ctx, method, addr, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	if observe != nil && req.Body != nil {
		req.Body = &watchedBody{ReadCloser: req.Body, progress: observe}
		// Retain the standard transport's idempotent PUT replay behavior.
		if original := req.GetBody; original != nil {
			req.GetBody = func() (io.ReadCloser, error) {
				b, err := original()
				if err != nil {
					return nil, err
				}
				return &watchedBody{ReadCloser: b, progress: observe}, nil
			}
		}
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	if typ != "" {
		req.Header.Set("Content-Type", typ)
	}
	resp, e := r.client.Do(req)
	if e != nil {
		return nil, e
	}
	if r.recordPeer && resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		sum := sha256.Sum256(resp.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo)
		r.peerPin = hex.EncodeToString(sum[:])
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		ae := &APIError{HTTP: resp.StatusCode}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(ae)
		return nil, ae
	}
	return resp, nil
}
func (r *Remote) call(ctx context.Context, method, path string, q url.Values, input, output any) error {
	var b []byte
	var e error
	typ := ""
	if input != nil {
		b, e = json.Marshal(input)
		if e != nil {
			return e
		}
		typ = "application/json"
	}
	return r.callBytes(ctx, method, path, q, b, typ, output, nil)
}

func (r *Remote) callBytes(ctx context.Context, method, path string, q url.Values, b []byte, typ string, output any, observe bodyObserver) error {
	resp, e := r.requestObserved(ctx, method, path, q, b, typ, observe)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if output == nil {
		_, e = io.Copy(io.Discard, resp.Body)
		return e
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 12<<20)).Decode(output)
}
func (r *Remote) eachPage(ctx context.Context, path string, q url.Values, visit func(Page) error) error {
	if q == nil {
		q = url.Values{}
	}
	seen := map[string]bool{}
	for {
		var page Page
		if e := r.call(ctx, "GET", path, q, nil, &page); e != nil {
			return e
		}
		if err := visit(page); err != nil {
			return err
		}
		if page.NextCursor == nil {
			return nil
		}
		cur := *page.NextCursor
		if cur == "" || seen[cur] {
			return errors.New("重复或无效分页游标")
		}
		seen[cur] = true
		q.Set("cursor", cur)
	}
}

func (r *Remote) pages(ctx context.Context, path string, q url.Values) (Page, error) {
	out := Page{Items: []json.RawMessage{}}
	err := r.eachPage(ctx, path, q, func(p Page) error {
		out.Items = append(out.Items, p.Items...)
		return nil
	})
	return out, err
}
func (r *Remote) Units(ctx context.Context) (Page, error) { return r.pages(ctx, "/units", nil) }
func (r *Remote) unitList(ctx context.Context, out *[]unitInfo) error {
	p, e := r.Units(ctx)
	if e != nil {
		return e
	}
	for _, m := range p.Items {
		var u unitInfo
		if e = json.Unmarshal(m, &u); e != nil {
			return e
		}
		*out = append(*out, u)
	}
	return nil
}
func (r *Remote) History(ctx context.Context, unit string) (Page, error) {
	if e := validID(unit); e != nil {
		return Page{}, e
	}
	return r.pages(ctx, "/units/"+unit+"/snapshots", nil)
}
func (r *Remote) Entries(ctx context.Context, unit, snapshot, path string) (Page, error) {
	if e := validID(unit); e != nil {
		return Page{}, e
	}
	q := url.Values{"path": {path}}
	if snapshot != "" {
		if e := validID(snapshot); e != nil {
			return Page{}, e
		}
		q.Set("snapshotId", snapshot)
	}
	return r.pages(ctx, "/units/"+unit+"/entries", q)
}
