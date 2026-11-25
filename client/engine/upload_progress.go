package engine

import (
	"bytes"
	"context"
	"database/sql"
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
	paths                int64
}
type chunkUse struct {
	file  *uploadFile
	bytes int64
}
type uploadTracker struct {
	*transfer
	mu       sync.Mutex
	rawRead  map[objectID]int64
	uses     map[string][]chunkUse
	files    [][]Offer
	metadata []Offer
}

// Build the path map from frozen manifests, never the current working tree.
// Identical files use their first path. The parent delta prunes unchanged
// directories/files. Shared and repeated chunks advance each affected file.
func (s *Store) uploadTracker(ctx context.Context, t *transfer, snapshot Snapshot, full bool) (*uploadTracker, error) {
	u := &uploadTracker{transfer: t, rawRead: make(map[objectID]int64), uses: make(map[string][]chunkUse)}
	rows, err := s.db.QueryContext(ctx, "SELECT o.kind,o.digest,o.size"+snapshotObjectDelta, snapshot.ID, full)
	if err != nil {
		return nil, err
	}
	pending := map[objectfmt.Ref]int{}
	for rows.Next() {
		var ref objectfmt.Ref
		var size int
		if err = rows.Scan(&ref.Kind, &ref.Digest, &size); err != nil {
			rows.Close()
			return nil, err
		}
		pending[ref] = size
		if ref.Kind != "chunk" {
			u.metadata = append(u.metadata, Offer{Kind: ref.Kind, Digest: ref.Digest, Size: size})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// One visit per distinct tree/file digest. Tree multiplicities are applied
	// afterwards, so two directory entries pointing at the same subtree count
	// as two complete logical directories without walking it twice.
	type tree struct {
		bytes, files, paths int64
		dirs                []*tree
		incomplete          []*uploadFile
	}
	trees := map[string]*tree{}
	files := map[string]*uploadFile{}
	var postorder []*tree
	uncached := map[string]*tree{}
	var walk func(string, string) (*tree, error)
	walk = func(prefix, digest string) (*tree, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if prev := trees[digest]; prev != nil {
			return prev, nil
		}
		d := &tree{}
		if _, changed := pending[objectfmt.Ref{Kind: "tree", Digest: digest}]; !changed {
			err := s.db.QueryRowContext(ctx, "SELECT bytes,files FROM tree_totals WHERE digest=?", digest).Scan(&d.bytes, &d.files)
			if err == nil {
				trees[digest] = d
				return d, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		entries, err := objectfmt.ReadDir(digest, s.load)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := path.Join(prefix, e.Name)
			switch e.Kind {
			case "directory":
				child, err := walk(name, e.Digest)
				if err != nil {
					return nil, err
				}
				d.bytes += child.bytes
				d.files += child.files
				d.dirs = append(d.dirs, child)
			case "file":
				f := files[e.Digest]
				if f == nil {
					info, err := objectfmt.ReadFile(e.Digest, s.load)
					if err != nil {
						return nil, err
					}
					f = &uploadFile{path: name, total: info.Size, done: info.Size}
					files[e.Digest] = f
					var owned []Offer
					for i, ref := range info.Chunks {
						if _, ok := pending[ref]; !ok {
							continue
						}
						size := min(int64(objectfmt.ChunkSize), info.Size-int64(i)*objectfmt.ChunkSize)
						f.done -= size
						uses := u.uses[ref.Digest]
						if len(uses) == 0 {
							owned = append(owned, Offer{ref.Kind, ref.Digest, int(size)})
						}
						if len(uses) > 0 && uses[len(uses)-1].file == f {
							uses[len(uses)-1].bytes += size
						} else {
							u.uses[ref.Digest] = append(uses, chunkUse{f, size})
						}
					}
					if len(owned) > 0 {
						u.files = append(u.files, owned)
					}
				}
				d.files++
				d.bytes += f.total
				if f.done < f.total {
					d.incomplete = append(d.incomplete, f)
				}
			}
		}
		trees[digest] = d
		uncached[digest] = d
		postorder = append(postorder, d)
		return d, nil
	}
	root, err := walk("", snapshot.Root)
	if err != nil {
		return nil, err
	}
	root.paths = 1
	for i := len(postorder) - 1; i >= 0; i-- {
		d := postorder[i]
		for _, child := range d.dirs {
			child.paths += d.paths
		}
		for _, f := range d.incomplete {
			f.paths += d.paths
		}
	}
	done, count := root.bytes, root.files
	for _, f := range files {
		done -= (f.total - f.done) * f.paths
		count -= f.paths
	}
	t.setProject(root.bytes, done, root.files, count)
	// Cache immutable subtree totals for unchanged directories of later
	// snapshots. No cache is needed to calculate the first/legacy snapshot.
	if len(uncached) > 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		stmt, err := tx.PrepareContext(ctx, "INSERT OR IGNORE INTO tree_totals(digest,bytes,files) VALUES(?,?,?)")
		if err != nil {
			return nil, err
		}
		defer stmt.Close()
		for digest, d := range uncached {
			if _, err := stmt.ExecContext(ctx, digest, d.bytes, d.files); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	for ref, size := range pending {
		if ref.Kind == "chunk" && len(u.uses[ref.Digest]) == 0 {
			u.metadata = append(u.metadata, Offer{Kind: ref.Kind, Digest: ref.Digest, Size: size})
		}
	}
	return u, nil
}

func (u *uploadTracker) confirm(id objectID) {
	if id.Kind != "chunk" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	advance, completed := -u.rawRead[id], int64(0)
	if n := u.rawRead[id]; n != 0 {
		u.uses[id.Digest][0].file.sending -= n
		delete(u.rawRead, id)
	}
	for _, use := range u.uses[id.Digest] {
		advance += use.bytes * use.file.paths
		use.file.done += use.bytes
		if use.file.done == use.file.total {
			completed += use.file.paths
			u.finishFile(use.file.path)
		}
	}
	u.advanceProject(advance, completed)
	delete(u.uses, id.Digest)
}

type uploadSegment struct {
	start, end int64
	size       int
	id         objectID
	path       string
}
type bodyObserver func(offset int64, n int)

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
		u.observeChunk(id, offset+int64(n), int64(n), size)
	}
}

func (u *uploadTracker) observeChunk(id objectID, offset, delta int64, size int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	uses := u.uses[id.Digest]
	if len(uses) == 0 {
		return
	}
	f := uses[0].file
	old := u.rawRead[id]
	read := max(old, min(int64(size), offset))
	u.rawRead[id] = read
	f.sending += read - old
	u.advanceProject(read-old, 0)
	u.file(f.path, f.done+f.sending, f.total, delta)
}

func (u *uploadTracker) batchObserver(batch []uploadItem, body []byte) (bodyObserver, error) {
	// json.Marshal encodes []byte as an unescaped base64 string. Track only
	// the bytes occupied by that field, preserving the existing batch wire
	// format, limits and small-file throughput.
	const marker = `"data":"`
	next := 0
	var segments []uploadSegment
	u.mu.Lock()
	defer u.mu.Unlock()
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
		segments = append(segments, uploadSegment{int64(start), int64(next), len(item.Data), objectID{item.Kind, item.Digest}, f.path})
	}
	if len(segments) == 0 {
		return nil, nil
	}
	first := segments[0]
	f := u.uses[first.id.Digest][0].file
	u.file(f.path, f.done+f.sending, f.total, 0)
	lastPath := first.path
	return func(offset int64, n int) {
		end := offset + int64(n)
		for _, seg := range segments {
			if seg.start >= end {
				break
			}
			start, stop := max(offset, seg.start)-seg.start, min(end, seg.end)-seg.start
			if start >= stop {
				continue
			}
			// A batch sends its members in sequence on one worker.
			if lastPath != seg.path {
				u.finishFile(lastPath)
				lastPath = seg.path
			}
			start = min(int64(seg.size), start*3/4)
			stop = min(int64(seg.size), stop*3/4)
			u.observeChunk(seg.id, stop, stop-start, seg.size)
		}
	}, nil
}
