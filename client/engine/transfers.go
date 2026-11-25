package engine

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type TransferStatus struct {
	ID             string `json:"id"`
	Direction      string `json:"direction"`
	Unit           string `json:"unitId"`
	Snapshot       string `json:"snapshotId"`
	Phase          string `json:"phase"`
	Path           string `json:"path"`
	FileBytes      int64  `json:"fileBytes"`
	FileTotal      int64  `json:"fileTotal"`
	BytesPerSecond int64  `json:"bytesPerSecond"`
}

const progressInterval = 350 * time.Millisecond
const progressExpiry = 12 * time.Second

// Each operation has a separate heartbeat file so a foreground CLI, the
// background agent and multiple downloads can all be observed independently.
func ActiveTransfers(home string) ([]TransferStatus, error) {
	items := []TransferStatus{}
	root, err := clientHome(home)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "transfers")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	for _, ent := range entries {
		name := ent.Name()
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !idPattern.MatchString(id) || !ent.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, name)
		st, err := ent.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if time.Since(st.ModTime()) > progressExpiry {
			// A suspended writer can re-publish its unique ID on its next tick.
			_ = os.Remove(path)
			continue
		}
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var item TransferStatus
		if json.Unmarshal(raw, &item) == nil && item.ID == id {
			items = append(items, item)
		}
	}
	return items, nil
}

type transfer struct {
	mu         sync.Mutex
	status     TransferStatus
	payload    int64
	path       string
	stop, done chan struct{}
}

func startTransfer(home, direction, unit, snapshot string) *transfer {
	t := &transfer{status: TransferStatus{ID: newID(), Direction: direction, Unit: unit, Snapshot: snapshot, Phase: "preparing"}}
	if home == "" {
		return t
	}
	dir := filepath.Join(home, "transfers")
	if os.MkdirAll(dir, 0700) != nil {
		return t
	}
	t.path = filepath.Join(dir, t.status.ID+".json")
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	t.publish(t.status)
	go t.run()
	return t
}

func (t *transfer) run() {
	defer close(t.done)
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	type sample struct {
		at    time.Time
		bytes int64
	}
	// Rate is file payload consumed by the HTTP transport over the last
	// two seconds. Idle or stalled connections fall back to zero.
	samples := []sample{{time.Now(), 0}}
	for {
		select {
		case <-t.stop:
			return
		case now := <-ticker.C:
			t.mu.Lock()
			cur, n := t.status, t.payload
			t.mu.Unlock()
			for len(samples) > 1 && now.Sub(samples[1].at) >= 2*time.Second {
				samples = samples[1:]
			}
			elapsed := now.Sub(samples[0].at).Seconds()
			if elapsed > 0 {
				cur.BytesPerSecond = int64(float64(n-samples[0].bytes) / elapsed)
			}
			samples = append(samples, sample{now, n})
			t.publish(cur)
		}
	}
}

func (t *transfer) publish(s TransferStatus) {
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	// The writer is unique per ID. A crash may leave a .tmp file, which is
	// never considered an active transfer.
	tmp := t.path + ".tmp"
	if err = os.WriteFile(tmp, raw, 0600); err == nil {
		_ = os.Rename(tmp, t.path)
	}
}

func (t *transfer) close() {
	if t.stop == nil {
		return
	}
	close(t.stop)
	<-t.done
	_ = os.Remove(t.path)
	_ = os.Remove(t.path + ".tmp")
}

func (t *transfer) phase(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Phase = name
	t.status.Path = ""
	t.status.FileBytes = 0
	t.status.FileTotal = 0
}

func (t *transfer) file(path string, completed, total, wireDelta int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status.Phase = "transferring"
	t.status.Path = path
	t.status.FileBytes = min(total, completed)
	t.status.FileTotal = total
	t.payload += wireDelta
}

type watchedBody struct {
	io.ReadCloser
	offset   int64
	progress func(offset int64, n int)
}

func (b *watchedBody) Read(buf []byte) (int, error) {
	n, err := b.ReadCloser.Read(buf)
	if n > 0 {
		b.progress(b.offset, n)
		b.offset += int64(n)
	}
	return n, err
}
func (t *transfer) download(body io.ReadCloser, path string, total int64) io.ReadCloser {
	t.file(path, 0, total, 0)
	return &watchedBody{ReadCloser: body, progress: func(offset int64, n int) { t.file(path, offset+int64(n), total, int64(n)) }}
}
