package engine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Exercise DAV against a real paired TLS NAS, including the pinned version after
// another publication, range fetching, and the local authorization boundary.
func TestWorkspaceDAVServesOnlyApprovedPinnedFile(t *testing.T) {
	f := setup(t)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	name, old := "记录 & 预算.txt", "pinned version"
	makeFile(t, f.source, name, old)
	v, err := f.s.Snapshot(ctx, f.unit, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(ctx, f.unit, v.ID); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.source, name), []byte("newer version"), 0600); err != nil {
		t.Fatal(err)
	}
	newer, err := f.s.Snapshot(ctx, f.unit, "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(ctx, f.unit, newer.ID); err != nil {
		t.Fatal(err)
	}
	// A connection-local table isolates these transport tests from filesystem
	// operations and staging journals managed by the workspace archiver.
	_, err = f.s.db.Exec(`CREATE TEMP TABLE workspace_archives(slot TEXT,unit TEXT,snapshot TEXT,member TEXT,sha TEXT,size INTEGER,device TEXT,phase TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	slot := strings.Repeat("4", 32)
	_, err = f.s.db.Exec(`INSERT INTO workspace_archives SELECT ?,?,?,?,?,?,device,'online' FROM account WHERE id=1`, slot, f.unit, v.ID, name, fmt.Sprintf("%x", sha256.Sum256([]byte(old))), len(old))
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan WorkspaceVolumeReady, 1)
	done := make(chan error, 1)
	go func() {
		done <- f.s.ServeWorkspaceVolume(ctx, func(r WorkspaceVolumeReady) error { ready <- r; return nil })
	}()
	var volume WorkspaceVolumeReady
	select {
	case volume = <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("no ready response")
	}
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	member := slot + "/" + url.PathEscape(name)
	request := func(method, p string, headers map[string]string, want int) string {
		t.Helper()
		req, err := http.NewRequest(method, volume.URL+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s %s: %d %v %s", method, p, resp.StatusCode, err, b)
		}
		return string(b)
	}
	if root := request("PROPFIND", "", map[string]string{"Depth": "1"}, 207); strings.Contains(root, slot) || strings.Contains(root, name) {
		t.Fatal("leaked a slot to root listing")
	}
	if children := request("PROPFIND", slot+"/", map[string]string{"Depth": "1"}, 207); !strings.Contains(children, "记录 &amp; 预算.txt") {
		t.Fatal(children)
	}
	if got := request("GET", member, nil, 200); got != old {
		t.Fatal(got)
	}
	if got := request("GET", member, map[string]string{"Range": "bytes=2-6"}, 206); got != old[2:7] {
		t.Fatal(got)
	}
	request("GET", member, map[string]string{"Range": "bytes=218-"}, 416)
	request("GET", member, map[string]string{"Origin": "https://example.org"}, 404)
	request("PROPFIND", strings.Repeat("3", 32)+"/", nil, 404)
	request("GET", slot+"/another.txt", nil, 404)
	request("DELETE", member, nil, 405)
	if runtime.GOOS == "darwin" && os.Getenv("MOONFOX_NATIVE_DAV_TEST") == "1" {
		cmd := exec.CommandContext(ctx, "/sbin/mount_webdav", "-o", "rdonly,nobrowse", "-v", "MoonFoxIsolatedTest", volume.URL, volume.MountPoint)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("mount: %v %s", err, output)
		}
		defer func() {
			if out, err := exec.Command("/sbin/umount", volume.MountPoint).CombinedOutput(); err != nil {
				t.Errorf("unmount: %v %s", err, out)
			}
		}()
		original := filepath.Join(t.TempDir(), "原位置")
		if err := os.Symlink(filepath.Join(volume.MountPoint, slot, name), original); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(original)
		if err != nil || string(data) != old {
			t.Fatalf("native pinned content %q %v", data, err)
		}
		if files, err := os.ReadDir(volume.MountPoint); err != nil || len(files) != 0 {
			t.Fatalf("OS enumerated unlisted slots %v %v", files, err)
		}
	}

	if _, err = f.s.db.Exec("UPDATE workspace_archives SET device=?", newID()); err != nil {
		t.Fatal(err)
	}
	request("GET", member, nil, 404)
	if _, err = f.s.db.Exec("UPDATE workspace_archives SET device=(SELECT device FROM account WHERE id=1),phase='local'"); err != nil {
		t.Fatal(err)
	}
	request("GET", member, nil, 404)

	var device string
	if err = f.s.db.QueryRow("SELECT device FROM account WHERE id=1").Scan(&device); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec("UPDATE workspace_archives SET phase='online'"); err != nil {
		t.Fatal(err)
	}
	if err = f.nas.Revoke(device); err != nil {
		t.Fatal(err)
	}
	request("GET", member, nil, 404)
}
