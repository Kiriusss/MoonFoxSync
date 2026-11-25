package engine

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func codexFixture(t *testing.T) fixture {
	t.Helper()
	f := setup(t)
	var device string
	if err := f.s.db.QueryRow("SELECT device FROM account").Scan(&device); err != nil {
		t.Fatal(err)
	}
	unit, err := f.nas.AddUnit("Codex history test")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.nas.Grant(unit, device, "sync"); err != nil {
		t.Fatal(err)
	}
	f.unit, f.source = unit, t.TempDir()
	if _, err = f.s.bindSource(context.Background(), unit, f.source, "codex", false, ""); err != nil {
		t.Fatal(err)
	}
	return f
}

func writeCodexFixture(t *testing.T, root, member, body string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(member))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexHistoryRoundTrip(t *testing.T) {
	f := codexFixture(t)
	ctx := context.Background()
	const live = "sessions/2024/02/29/rollout-live.jsonl"
	// One CRLF-terminated record spans multiple 4 MiB chunks. Its unfinished
	// successor exceeds the reverse reader's buffer and must remain on the source.
	huge := "{\"msg\":\"" + strings.Repeat("x", objectfmt.ChunkSize+39) + "\"}\r\n"
	half := "{\"msg\":\"" + strings.Repeat("t", 130<<10)
	want := map[string]string{
		live:                                  huge,
		"history.jsonl":                       "{\"text\":\"一\"}\n",
		"session_index.jsonl":                 "{\"id\":\"test\"}\n",
		"archived_sessions/rollout-old.jsonl": "{\"old\":1}\n",
		"archived_sessions/2023/12/31/rollout-older.jsonl": "{\"old\":2}\n",
	}
	for name, body := range want {
		writeCodexFixture(t, f.source, name, body)
	}
	livePath := writeCodexFixture(t, f.source, live, huge+half)
	for _, name := range []string{
		"auth.json", "config.toml", "state.db", "rollout-top.jsonl", "plugins/2024/02/29/rollout-no.jsonl", "sessions/rollout-flat.jsonl",
		"sessions/2023/02/29/rollout-baddate.jsonl", "sessions/2024/02/29/debug.jsonl", "sessions/2024/02/29/token",
		"archived_sessions/nested/rollout-no.jsonl", "sessions/2024/02/29/rollout-empty.jsonl",
	} {
		writeCodexFixture(t, f.source, name, "private fixture\n")
	}
	writeCodexFixture(t, f.source, "sessions/2024/02/29/rollout-empty.jsonl", "incomplete")
	secret := filepath.Join(f.source, "auth.json")
	dir := filepath.Dir(livePath)
	if err := os.Link(secret, filepath.Join(dir, "rollout-linked.jsonl")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" { // symlink creation can require Windows developer mode.
		if err := os.Symlink(secret, filepath.Join(dir, "rollout-secret.jsonl")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dir, filepath.Join(filepath.Dir(dir), "28")); err != nil {
			t.Fatal(err)
		}
	}
	v1, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(ctx, f.unit, v1.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.s.db.QueryRow("SELECT COUNT(*) FROM codex_file_cache").Scan(&count); err != nil || count != len(want)+1 {
		t.Fatal(count, err)
	}
	// An unchanged scan cannot replace any cache rows, even for the incomplete file.
	if _, err = f.s.db.Exec("CREATE TRIGGER unchanged_codex BEFORE UPDATE ON codex_file_cache BEGIN SELECT RAISE(ABORT,'unexpected source recapture'); END"); err != nil {
		t.Fatal(err)
	}
	again, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil || again.ID != v1.ID {
		t.Fatal(again, err)
	}
	if _, err = f.s.db.Exec("DROP TRIGGER unchanged_codex"); err != nil {
		t.Fatal(err)
	}
	appendText := func(s string) {
		file, err := os.OpenFile(livePath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteString(s)
		closed := file.Close()
		if err != nil || closed != nil {
			t.Fatal(err, closed)
		}
	}
	appendText("y")
	again, err = f.s.Snapshot(ctx, f.unit, "")
	if err != nil || again.ID != v1.ID {
		t.Fatalf("half line created snapshot %v %v", again, err)
	}
	appendText("\"}\n")
	v2, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil || v2.ID == v1.ID || v2.Parent == nil || *v2.Parent != v1.ID {
		t.Fatal(v2, err)
	}
	if _, err = f.s.Sync(ctx, f.unit, v2.ID); err != nil {
		t.Fatal(err)
	}
	remote, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []Snapshot{v1, v2} {
		out := filepath.Join(t.TempDir(), "restored")
		report, err := remote.Restore(ctx, f.unit, version.ID, "", out)
		if err != nil || report.Files != len(want) {
			t.Fatal(report, err)
		}
		err = filepath.WalkDir(out, func(path string, entry fs.DirEntry, e error) error {
			if e != nil || entry.IsDir() {
				return e
			}
			rel, e := filepath.Rel(out, path)
			if e != nil {
				return e
			}
			name := filepath.ToSlash(rel)
			expected, ok := want[name]
			if !ok {
				t.Fatalf("unexpected restored path %q", name)
			}
			if name == live && version.ID == v2.ID {
				expected += half + "y\"}\n"
			}
			body, e := os.ReadFile(path)
			if e == nil && !bytes.Equal(body, []byte(expected)) {
				t.Fatalf("wrong restored body %q: %d bytes", name, len(body))
			}
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexEmptyAndCacheRepair(t *testing.T) {
	f := codexFixture(t)
	ctx := context.Background()
	checkEmpty := func() {
		t.Helper()
		_, err := f.s.Snapshot(ctx, f.unit, "")
		if !errors.Is(err, ErrCodexHistoryEmpty) {
			t.Fatal(err)
		}
	}
	checkEmpty()
	p := writeCodexFixture(t, f.source, "history.jsonl", "unfinished")
	checkEmpty()
	var cached string
	if err := f.s.db.QueryRow("SELECT digest FROM codex_file_cache WHERE path='history.jsonl'").Scan(&cached); err != nil || cached != "" {
		t.Fatal(cached, err)
	}
	queue, err := f.s.Queue(ctx)
	if err != nil || len(queue) != 0 {
		t.Fatal(queue, err)
	}
	writeCodexFixture(t, f.source, "history.jsonl", "{\"a\":1}\n")
	v1, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.db.QueryRow("SELECT digest FROM codex_file_cache WHERE path='history.jsonl'").Scan(&cached); err != nil {
		t.Fatal(err)
	}
	info, err := objectfmt.ReadFile(cached, f.s.load)
	if err != nil {
		t.Fatal(err)
	}
	chunk := f.s.objectPath(info.Chunks[0])
	if err = os.Remove(chunk); err != nil {
		t.Fatal(err)
	}
	again, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil || again.ID != v1.ID {
		t.Fatal(again, err)
	}
	if _, err = os.Stat(chunk); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	writeCodexFixture(t, f.source, "history.jsonl", "{\"a\":2}\n")
	if err = os.Chtimes(p, time.Now(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	newer, err := f.s.Snapshot(ctx, f.unit, "")
	if err != nil || newer.ID == v1.ID {
		t.Fatal("same-length backdated rewrite incorrectly reused", newer, err)
	}
}

func TestCodexCompleteEndHonorsInitialSize(t *testing.T) {
	ctx := context.Background()
	data := []byte("first\r\n" + strings.Repeat("x", 64<<10) + "\nlast\n")
	r := bytes.NewReader(data)
	for _, x := range []struct{ sample, want int64 }{{0, 0}, {5, 0}, {7, 7}, {7 + 64<<10, 7}, {8 + 64<<10, 8 + 64<<10}, {int64(len(data)), int64(len(data))}} {
		end, err := codexCompleteEnd(ctx, r, x.sample)
		if err != nil || end != x.want {
			t.Fatal(x, end, err)
		}
	}
}

func TestCodexSourceLinkCannotReuseOriginalIdentity(t *testing.T) {
	f := codexFixture(t)
	writeCodexFixture(t, f.source, "history.jsonl", "{}\n")
	moved := filepath.Join(filepath.Dir(f.source), "moved-codex-history")
	if err := os.Rename(f.source, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(moved) })
	if err := os.Symlink(moved, f.source); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("creating symlinks requires developer mode")
		}
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := f.s.Snapshot(ctx, f.unit, ""); err == nil {
		t.Fatal("captured a symlink to the original directory")
	}
	queue, err := f.s.Queue(ctx)
	if err != nil || len(queue) != 0 {
		t.Fatal(queue, err)
	}
}

func TestCodexBackupDuringAppend(t *testing.T) {
	f := codexFixture(t)
	const row = "{\"text\":\"每次一行\"}\n"
	initial := strings.Repeat(row, 4096)
	path := writeCodexFixture(t, f.source, "history.jsonl", initial)
	w, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	stop := make(chan struct{})
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		for n := 0; ; n++ {
			if _, err := w.WriteString(row); err != nil {
				finished <- err
				return
			}
			if n == 0 {
				close(started)
			}
			select {
			case <-stop:
				finished <- nil
				return
			case <-time.After(200 * time.Microsecond):
			}
		}
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatal(err)
	}
	ctx := context.Background()
	v, err := f.s.Snapshot(ctx, f.unit, "")
	close(stop)
	if writeErr := <-finished; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Sync(ctx, f.unit, v.ID); err != nil {
		t.Fatal(err)
	}
	remote, err := f.s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "restored")
	if _, err = remote.Restore(ctx, f.unit, v.ID, "", out); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(out, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) < len(initial) || len(saved)%len(row) != 0 || !bytes.HasPrefix(current, saved) {
		t.Fatal("active-writer backup contained a partial or corrupt record")
	}
}
