package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path"
	"sync"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

const snapshotObjectDelta = ` FROM snapshot_objects o WHERE o.snapshot=? AND (? OR NOT EXISTS
 (SELECT 1 FROM snapshots c JOIN snapshots p ON p.id=c.parent AND p.receipt IS NOT NULL
 JOIN snapshot_objects x ON x.snapshot=p.id AND x.kind=o.kind AND x.digest=o.digest WHERE c.id=o.snapshot))`

type uploadFile struct {
	path                 string
	total, done, sending int64
}
type chunkUse struct {
	file  *uploadFile
	bytes int64
}
type uploadTracker struct {
	*transfer
	mu      sync.Mutex
	rawRead map[objectID]int64
	uses    map[string][]chunkUse
}

// Build the path map from frozen manifests, never the current working tree.
// Identical files use their first path. The parent delta prunes unchanged
// directories/files. Shared and repeated chunks advance each affected file.
func (s *Store) uploadTracker(ctx context.Context, t *transfer, snapshot Snapshot, full bool) (*uploadTracker, error) {
	u := &uploadTracker{transfer: t, rawRead: make(map[objectID]int64), uses: make(map[string][]chunkUse)}
	rows, err := s.db.QueryContext(ctx, "SELECT o.kind,o.digest"+snapshotObjectDelta, snapshot.ID, full)
	if err != nil {
		return nil, err
	}
	pending := map[objectfmt.Ref]bool{}
	for rows.Next() {
		var ref objectfmt.Ref
		if err = rows.Scan(&ref.Kind, &ref.Digest); err != nil {
			rows.Close()
			return nil, err
		}
		pending[ref] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var walk func(string, string) error
	walk = func(prefix, digest string) error {
		key := objectfmt.Ref{Kind: "tree", Digest: digest}
		if !pending[key] {
			return nil
		}
		delete(pending, key)
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := objectfmt.ReadDir(digest, s.load)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := path.Join(prefix, e.Name)
			switch e.Kind {
			case "directory":
				if err = walk(name, e.Digest); err != nil {
					return err
				}
			case "file":
				key := objectfmt.Ref{Kind: "file", Digest: e.Digest}
				if !pending[key] {
					continue
				}
				delete(pending, key)
				info, err := objectfmt.ReadFile(e.Digest, s.load)
				if err != nil {
					return err
				}
				f := &uploadFile{path: name, total: info.Size, done: info.Size}
				for i, ref := range info.Chunks {
					if !pending[ref] {
						continue
					}
					size := min(int64(objectfmt.ChunkSize), info.Size-int64(i)*objectfmt.ChunkSize)
					f.done -= size
					uses := u.uses[ref.Digest]
					if len(uses) > 0 && uses[len(uses)-1].file == f {
						uses[len(uses)-1].bytes += size
					} else {
						u.uses[ref.Digest] = append(uses, chunkUse{f, size})
					}
				}
			}
		}
		return nil
	}
	if err = walk("", snapshot.Root); err != nil {
		return nil, err
	}
	return u, nil
}

func (u *uploadTracker) confirm(id objectID) {
	if id.Kind != "chunk" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if n := u.rawRead[id]; n != 0 {
		u.uses[id.Digest][0].file.sending -= n
		delete(u.rawRead, id)
	}
	for _, use := range u.uses[id.Digest] {
		use.file.done += use.bytes
	}
	delete(u.uses, id.Digest)
}

// Capture immutable offsets before issuing an HTTP request. A byte shared by
// multiple paths contributes to network speed only once. All users receive
// the remaining logical progress when the object is acknowledged.
type uploadSegment struct {
	start, end        int64
	path              string
	base, total, size int64
	encoded           bool
}
type bodyObserver func(offset int64, n int)

func (u *uploadTracker) observe(segments []uploadSegment) bodyObserver {
	return func(offset int64, n int) {
		end := offset + int64(n)
		for _, s := range segments {
			if s.start >= end {
				break
			}
			start, stop := max(offset, s.start)-s.start, min(end, s.end)-s.start
			if start >= stop {
				continue
			}
			if s.encoded {
				// Four encoded bytes carry up to three bytes of file content.
				start = min(s.size, start*3/4)
				stop = min(s.size, stop*3/4)
			}
			u.file(s.path, s.base+stop, s.total, stop-start)
		}
	}
}

func (u *uploadTracker) rawObserver(id objectID, size int) bodyObserver {
	u.mu.Lock()
	defer u.mu.Unlock()
	uses := u.uses[id.Digest]
	if id.Kind != "chunk" || len(uses) == 0 {
		return nil
	}
	f := uses[0].file
	u.file(f.path, f.done+f.sending, f.total, 0)
	return func(offset int64, n int) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if _, pending := u.uses[id.Digest]; !pending {
			return
		}
		// A transport retry can read the same bytes again. Count wire bytes
		// each time, but count a logical file byte only once.
		old := u.rawRead[id]
		read := max(old, min(int64(size), offset+int64(n)))
		u.rawRead[id] = read
		f.sending += read - old
		u.file(f.path, f.done+f.sending, f.total, int64(n))
	}
}

func (u *uploadTracker) batchObserver(batch []uploadItem, body []byte) (bodyObserver, error) {
	// json.Marshal encodes []byte as an unescaped base64 string. Track only
	// the bytes occupied by that field, preserving the existing batch wire
	// format, limits and small-file throughput.
	const marker = `"data":"`
	next := 0
	var segments []uploadSegment
	inBatch := map[*uploadFile]int64{}
	for _, item := range batch {
		pos := bytes.Index(body[next:], []byte(marker))
		if pos < 0 {
			return nil, errors.New("无法定位批量传输的文件数据")
		}
		start := next + pos + len(marker)
		next = start + base64.StdEncoding.EncodedLen(len(item.Data))
		if next >= len(body) || body[next] != '"' {
			return nil, errors.New("批量传输编码边界无效")
		}
		uses := u.uses[item.Digest]
		if item.Kind != "chunk" || len(uses) == 0 {
			continue
		}
		f := uses[0].file
		segments = append(segments, uploadSegment{int64(start), int64(next), f.path, f.done + inBatch[f], f.total, int64(len(item.Data)), true})
		for _, use := range uses {
			inBatch[use.file] += use.bytes
		}
	}
	if len(segments) == 0 {
		return nil, nil
	}
	first := segments[0]
	u.file(first.path, first.base, first.total, 0)
	return u.observe(segments), nil
}
