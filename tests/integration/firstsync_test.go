// Package integration tests the real NAS and desktop command-line programs on loopback.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type object = map[string]any

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func jsonBytes(t *testing.T, data any) []byte {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func certificate(t *testing.T, dir string) (*http.Client, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	k, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	write(t, certPath, certPEM)
	write(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: k}))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	trans := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(trans.CloseIdleConnections)
	return &http.Client{Transport: trans, Timeout: 10 * time.Second}, certPath, keyPath
}

func command(t *testing.T, binary string, env []string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(binary), args[0], err, stderr.String())
	}
	return b
}
func cli(t *testing.T, binary string, env []string, args ...string) object {
	t.Helper()
	data := command(t, binary, env, args...)
	var obj object
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("decode %s %s: %v", filepath.Base(binary), args[0], err)
	}
	return obj
}

func startNAS(t *testing.T, binary, config string) (string, func()) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binary, "serve", "--config", config)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 1)
	exited := make(chan error, 1)
	var log bytes.Buffer
	go func() {
		reader := bufio.NewScanner(stderr)
		for reader.Scan() {
			line := reader.Text()
			fmt.Fprintln(&log, line)
			parts := strings.Fields(line)
			if len(parts) >= 2 && parts[len(parts)-2] == "HTTPS" {
				select {
				case started <- parts[len(parts)-1]:
				default:
				}
			}
		}
		exited <- cmd.Wait()
	}()
	var once sync.Once
	stop := func() { once.Do(func() { _ = cmd.Process.Kill(); <-exited }) }
	select {
	case addr := <-started:
		t.Cleanup(stop)
		return addr, stop
	case err := <-exited:
		t.Fatalf("server startup: %v\n%s", err, log.String())
	case <-time.After(10 * time.Second):
		stop()
		t.Fatalf("server did not listen\n%s", log.String())
	}
	panic("unreachable")
}

func wire(t *testing.T, client *http.Client, token, method, url string, body any, headers map[string]string, want int) ([]byte, http.Header) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(jsonBytes(t, body))
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, req.URL.Path, r.StatusCode, want, data)
	}
	return data, r.Header
}

func TestFirstSync(t *testing.T) {
	if testing.Short() {
		t.Skip("runs compiled CLI binaries")
	}
	if runtime.GOOS == "windows" {
		t.Skip("uses an isolated Unix headless credential file")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(t.Context(), filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin+string(os.PathSeparator), "./cmd/kdrive-server", "./cmd/kdrive")
	build.Dir = root
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	server, client := filepath.Join(bin, "kdrive-server"), filepath.Join(bin, "kdrive")
	httpClient, certPath, keyPath := certificate(t, tmp)
	cfg := filepath.Join(tmp, "server.json")
	setConfig := func(addr string) {
		write(t, cfg, jsonBytes(t, object{"formatVersion": 1, "dataDir": filepath.Join(tmp, "nas"), "listen": []string{addr}, "tls": object{"certFile": certPath, "keyFile": keyPath}}))
	}
	setConfig("127.0.0.1:0")
	command(t, server, nil, "init", "--config", cfg)
	unit := cli(t, server, nil, "unit", "add", "--config", cfg, "--name", "project")["unitId"].(string)
	addr, stop := startNAS(t, server, cfg)
	endpoint := "https://" + addr
	pair := func(role string) ([]string, string) {
		home := filepath.Join(tmp, role)
		if err := os.Mkdir(home, 0700); err != nil {
			t.Fatal(err)
		}
		env := []string{"KDRIVE_HOME=" + home, "KDRIVE_TOKEN_FILE=" + filepath.Join(home, "token")}
		code := cli(t, server, nil, "pair", "issue", "--config", cfg, "--unit", unit, "--role", role)["code"].(string)
		codeFile := filepath.Join(home, "pair-code")
		write(t, codeFile, []byte(code))
		cli(t, client, env, "pair", "--server", endpoint, "--ca", certPath, "--code-file", codeFile)
		secret, err := os.ReadFile(filepath.Join(home, "token"))
		if err != nil {
			t.Fatal(err)
		}
		return env, strings.TrimSpace(string(secret))
	}
	writer, wt := pair("sync")
	reader, rt := pair("reader")

	src := filepath.Join(tmp, "source")
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, "a.txt")
	write(t, file, []byte("abc"))
	for _, p := range []string{file, filepath.Join(src, "empty")} {
		if err := os.Chtimes(p, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
			t.Fatal(err)
		}
	}
	cli(t, client, writer, "unit", "bind", unit, "--path", src, "--profile", "project")
	snap := cli(t, client, writer, "snapshot", unit, "--name", "first")
	sid := snap["snapshotId"].(string)
	const rootDigest = "1b3d5dd676cd805d5cf3e579682eb2901dd37c3c30d6ce82d368f9cbdc4d9d75"
	if snap["rootDigest"] != rootDigest {
		t.Fatalf("wrong fixture root: %v", snap["rootDigest"])
	}
	write(t, file, []byte("local edits after checkpoint"))
	history := func() []any { return cli(t, client, reader, "history", unit)["items"].([]any) }
	if len(history()) != 0 {
		t.Fatal("unpublished checkpoint visible on NAS")
	}
	pendingArgs := []string{"sync-pending", "--now", "--home-url", "https://192.168.1.1:7443", "--home-interface", "kdrive-no-such-interface"}
	for _, count := range []float64{1, 0} {
		summary := cli(t, client, writer, pendingArgs...)
		want := object{"total": count, "uploaded": count, "active": float64(0), "skipped": float64(0), "failed": float64(0), "errors": []any{}}
		if !reflect.DeepEqual(summary, want) {
			t.Fatalf("sync-pending: got %v, want %v", summary, want)
		}
	}
	syncArgs := []string{"sync", unit, "--snapshot", sid, "--now"}
	rec := cli(t, client, writer, syncArgs...)
	if rec["rootDigest"] != rootDigest || rec["outcome"] != "published" {
		t.Fatal("wrong receipt", rec)
	}
	if replay := cli(t, client, writer, syncArgs...); !reflect.DeepEqual(replay, rec) {
		t.Fatal("receipt changed")
	}
	unitURL := endpoint + "/v1/units/" + unit
	rangeBytes, h := wire(t, httpClient, rt, "GET", unitURL+"/content?path=a.txt&snapshotId="+sid, nil, map[string]string{"Range": "bytes=1-1"}, 206)
	if string(rangeBytes) != "b" || h.Get("Content-Range") != "bytes 1-1/3" || h.Get("ETag") != `"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"` {
		t.Fatalf("range mismatch: %q %v", rangeBytes, h)
	}
	// A second server-side checkpoint references the same immutable tree.
	// Replay finalize after a forced process restart to cover lost receipt recovery.
	checkpoint := object{"requestId": "00000000000000000000000008", "clientSnapshotId": "00000000000000000000000009", "scopeRevision": 1, "rootDigest": rootDigest, "capturedAt": "2026-11-25T00:00:00Z", "parentSnapshotId": sid, "expectedHead": sid, "label": "same content", "checkpoint": true, "links": []any{}, "attachments": []any{}}
	wire(t, httpClient, rt, "POST", unitURL+"/uploads", checkpoint, nil, 403)
	response, _ := wire(t, httpClient, wt, "POST", unitURL+"/uploads", checkpoint, nil, 201)
	var session struct {
		ID string `json:"uploadId"`
	}
	if err = json.Unmarshal(response, &session); err != nil || session.ID == "" {
		t.Fatalf("missing upload id: %v", err)
	}
	finalize := endpoint + "/v1/uploads/" + session.ID + "/finalize"
	before, _ := wire(t, httpClient, wt, "POST", finalize, nil, nil, 200)
	stop()
	setConfig(addr)
	_, _ = startNAS(t, server, cfg)
	after, _ := wire(t, httpClient, wt, "POST", finalize, nil, nil, 200)
	if !bytes.Equal(bytes.TrimSpace(before), bytes.TrimSpace(after)) {
		t.Fatal("server changed durable receipt after restart")
	}
	items := history()
	if len(items) != 2 {
		t.Fatalf("want two checkpoints, got %d", len(items))
	}
	dst := filepath.Join(tmp, "restored")
	cli(t, client, reader, "restore", unit, sid, "--to", dst)
	b, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(b) != "abc" {
		t.Fatal("wrong restored bytes", err)
	}
	info, err := os.Stat(filepath.Join(dst, "empty"))
	if err != nil || !info.IsDir() {
		t.Fatal("empty directory not restored", err)
	}
}
