package engine

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ProjectProgress struct {
	Bytes      int64 `json:"projectBytes"`
	Total      int64 `json:"projectTotal"`
	FilesDone  int64 `json:"projectFilesDone"`
	FilesTotal int64 `json:"projectFilesTotal"`
}

type TransferStatus struct {
	*ProjectProgress
	OperationID    string `json:"operationId,omitempty"`
	ProjectRate    int64  `json:"projectRate"`
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

// The top-level status is also readable by previous clients. New clients
// show the individual active paths recorded in the same atomic heartbeat.
type transferHeartbeat struct {
	TransferStatus
	Files []TransferStatus `json:"files,omitempty"`
}
type progressSample struct {
	at    time.Time
	bytes int64
}
type liveFile struct {
	status  TransferStatus
	payload int64
	samples []progressSample
}

func currentRate(now time.Time, n int64, samples *[]progressSample) int64 {
	for len(*samples) > 1 && now.Sub((*samples)[1].at) >= 2*time.Second {
		*samples = (*samples)[1:]
	}
	first := (*samples)[0]
	*samples = append(*samples, progressSample{now, n})
	if elapsed := now.Sub(first.at).Seconds(); elapsed > 0 {
		return int64(float64(n-first.bytes) / elapsed)
	}
	return 0
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
		var item transferHeartbeat
		if json.Unmarshal(raw, &item) == nil && item.ID == id {
			if len(item.Files) > 0 {
				items = append(items, item.Files...)
			} else {
				items = append(items, item.TransferStatus)
			}
		}
	}
	return items, nil
}

type transfer struct {
	project              *ProjectProgress
	highBytes, highFiles int64
	mu                   sync.Mutex
	status               TransferStatus
	files                map[string]*liveFile
	payload              int64
	path                 string
	stop, done           chan struct{}
}

func startTransfer(home, direction, unit, snapshot string) *transfer {
	t := &transfer{status: TransferStatus{ID: newID(), Direction: direction, Unit: unit, Snapshot: snapshot, Phase: "preparing"}}
	t.status.OperationID = t.status.ID
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
	t.publish(transferHeartbeat{TransferStatus: t.status})
	go t.run()
	return t
}

func (t *transfer) run() {
	defer close(t.done)
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	samples := []progressSample{{time.Now(), 0}}
	for {
		select {
		case <-t.stop:
			return
		case now := <-ticker.C:
			t.mu.Lock()
			h := transferHeartbeat{TransferStatus: t.status}
			h.BytesPerSecond = currentRate(now, t.payload, &samples)
			h.ProjectRate = h.BytesPerSecond
			if t.project != nil {
				p := *t.project
				t.highBytes = max(t.highBytes, p.Bytes)
				t.highFiles = max(t.highFiles, p.FilesDone)
				p.Bytes, p.FilesDone = t.highBytes, t.highFiles
				h.ProjectProgress = &p
			}
			for _, f := range t.files {
				item := f.status
				item.ProjectProgress = h.ProjectProgress
				item.ProjectRate = h.ProjectRate
				item.BytesPerSecond = currentRate(now, f.payload, &f.samples)
				h.Files = append(h.Files, item)
			}
			t.mu.Unlock()
			sort.Slice(h.Files, func(i, j int) bool { return h.Files[i].Path < h.Files[j].Path })
			t.publish(h)
		}
	}
}

func (t *transfer) publish(s transferHeartbeat) {
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

func (t *transfer) setProject(total, done, files, complete int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.project = &ProjectProgress{Bytes: done, Total: total, FilesDone: complete, FilesTotal: files}
}
func (t *transfer) advanceProject(bytes, files int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.project != nil {
		t.project.Bytes += bytes
		t.project.FilesDone += files
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
	clear(t.files)
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
	if t.files == nil {
		t.files = make(map[string]*liveFile)
	}
	f := t.files[path]
	if f == nil {
		f = &liveFile{samples: []progressSample{{time.Now(), 0}}}
		t.files[path] = f
	}
	f.status = t.status
	f.status.ID = t.status.ID + "/" + path
	f.payload += wireDelta
}
func (t *transfer) finishFile(path string) {
	t.mu.Lock()
	delete(t.files, path)
	t.mu.Unlock()
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
