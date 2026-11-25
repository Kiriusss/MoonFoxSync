package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

type Receipt struct {
	Unit        string  `json:"unitId"`
	Snapshot    string  `json:"snapshotId"`
	Root        string  `json:"rootDigest"`
	Outcome     string  `json:"outcome"`
	Head        *string `json:"headAtCommit"`
	CommittedAt string  `json:"committedAt"`
}
type objectID struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
}
type uploadItem struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Data   []byte `json:"data"`
}

func (s *Store) Sync(ctx context.Context, unit, id string) (*Receipt, error) {
	r, e := s.Remote(ctx)
	if e != nil {
		return nil, e
	}
	return s.syncUsing(ctx, r, unit, id)
}
func (s *Store) syncUsing(ctx context.Context, r *Remote, unit, id string) (*Receipt, error) {
	activity, lockErr := s.localLock(false)
	if lockErr != nil {
		return nil, lockErr
	}
	defer activity.Close()
	receipt, _, err := s.withUpload(ctx, unit, id, func(v Snapshot, request string) (*Receipt, error) {
		return s.sendSnapshot(ctx, r, v, request)
	})
	return receipt, err
}

var errUploadActive = errors.New("此版本正在传输")

// The caller holds the shared maintenance lock. Keep ownership from receipt lookup
// through transfer and accounting, across the agent and all foreground processes.
func (s *Store) withUpload(ctx context.Context, unit, id string, send func(Snapshot, string) (*Receipt, error)) (receipt *Receipt, reused bool, err error) {
	if err := validID(unit); err != nil {
		return nil, false, err
	}
	if err := validID(id); err != nil {
		return nil, false, err
	}
	project, err := s.projectLock(unit, false)
	if err != nil {
		return nil, false, err
	}
	defer project.Close()
	dir := filepath.Join(s.Home, "upload-locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, false, err
	}
	path := filepath.Join(dir, id)
	lock, err := cacheLock(path)
	if lockBusy(err) {
		return nil, false, errUploadActive
	}
	if err != nil {
		return nil, false, err
	}
	defer func() {
		lock.Close()
		// Only discard a lock file after publication. A later owner always
		// rechecks the durable receipt, even if it opened the old inode.
		if receipt != nil && err == nil {
			_ = os.Remove(path)
		}
	}()
	var v Snapshot
	var request string
	var ack []byte
	err = s.db.QueryRowContext(ctx, "SELECT "+snapCols+",request,receipt FROM snapshots WHERE id=? AND unit=?", id, unit).
		Scan(&v.ID, &v.Unit, &v.Parent, &v.Root, &v.Label, &v.Captured, &v.State, &request, &ack)
	if err != nil {
		return nil, false, err
	}
	if len(ack) > 0 {
		var saved Receipt
		err := json.Unmarshal(ack, &saved)
		return &saved, true, err
	}
	receipt, err = send(v, request)
	return receipt, false, err
}

func (s *Store) sendSnapshot(ctx context.Context, r *Remote, v Snapshot, request string) (*Receipt, error) {
	id, unit := v.ID, v.Unit
	var e error
	if v.Parent != nil {
		var status string
		e = s.db.QueryRowContext(ctx, "SELECT state FROM snapshots WHERE id=?", *v.Parent).Scan(&status)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if e == nil && status != "published" && status != "fork" {
			return nil, fmt.Errorf("请先同步父快照 %s", *v.Parent)
		}
	}
	progress := startTransfer(s.Home, "upload", unit, id)
	defer progress.close()
	path := "/units/" + unit + "/uploads"
	draft := map[string]any{"requestId": request, "clientSnapshotId": id, "parentSnapshotId": v.Parent, "expectedHead": v.Parent, "rootDigest": v.Root, "scopeRevision": 1, "label": v.Label, "capturedAt": v.Captured, "checkpoint": v.Label != "", "links": []any{}, "attachments": []any{}}
	if v.Parent != nil {
		var proof string
		err := s.db.QueryRowContext(ctx, "SELECT observed FROM project_proofs WHERE unit=? AND base=?", unit, *v.Parent).Scan(&proof)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			draft["baseContentDigest"] = proof
		}
	}
	var session struct {
		ID      string   `json:"uploadId"`
		Receipt *Receipt `json:"receipt"`
	}
	e = r.call(ctx, "POST", path, nil, draft, &session)
	var api *APIError
	if errors.As(e, &api) && api.Code == "SESSION_EXPIRED" {
		next := newID()
		if _, err := s.db.ExecContext(ctx, "UPDATE snapshots SET request=? WHERE id=? AND request=?", next, id, request); err != nil {
			return nil, err
		}
		return nil, errors.New("上传会话过期，重新运行 sync 可再次协商已保存的数据")
	}
	if e != nil {
		return nil, e
	}
	if e = validID(session.ID); e != nil {
		return nil, e
	}
	receipt := session.Receipt
	if receipt == nil {
		if e = s.pushObjects(ctx, r, progress, v, session.ID, false); e != nil {
			return nil, e
		}
		receipt = &Receipt{}
		finalize := func() error {
			progress.phase("finalizing")
			return r.commitCall(ctx, "POST", "/uploads/"+session.ID+"/finalize", nil, receipt)
		}
		e = finalize()
		if errors.As(e, &api) && api.Code == "OBJECTS_MISSING" && v.Parent != nil {
			// A previous local receipt can outlive the parent's visible history.
			// Re-offer the full manifest once, with the same session and policy:
			// the NAS requests bytes only for references it can no longer reuse.
			if e = s.pushObjects(ctx, r, progress, v, session.ID, true); e == nil {
				e = finalize()
			}
		}
		if e != nil {
			return nil, e
		}
	}
	if receipt.Unit != unit || receipt.Snapshot != id || receipt.Root != v.Root || (receipt.Outcome != "published" && receipt.Outcome != "fork") {
		return nil, errors.New("NAS 回执与本机冻结版本不一致")
	}
	data, e := json.Marshal(receipt)
	if e != nil {
		return nil, e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE snapshots SET state=?,receipt=? WHERE id=?", receipt.Outcome, data, id)
	return receipt, e
}
func (s *Store) pushObjects(ctx context.Context, r *Remote, progress *transfer, snapshot Snapshot, session string, full bool) error {
	progress.phase("preparing")
	tracker, err := s.uploadTracker(ctx, progress, snapshot, full)
	if err != nil {
		return err
	}
	settings, err := s.TransferSettings(ctx)
	if err != nil {
		return err
	}
	workers := min(settings.Uploads, len(tracker.files))
	if workers > 0 {
		ctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		// Files stay on the same worker. Each shared chunk has one owner, and
		// batches put consecutive small files on a single connection. Multiple
		// connections therefore upload independent files, even when all are tiny.
		var group sync.WaitGroup
		for lane := range workers {
			group.Go(func() {
				release, err := acquireFileSlot(ctx, s.Home, "upload", settings.Uploads)
				if err != nil {
					cancel(err)
					return
				}
				defer release()
				if err := s.sendFileLane(ctx, r, tracker, session, lane, workers, min(maxRawUploads, settings.Uploads/workers)); err != nil {
					cancel(err)
				}
			})
		}
		group.Wait()
		if err := context.Cause(ctx); err != nil {
			return err
		}
	}
	// Manifests are independent of their data on the wire. Finalization
	// references them only after every file and metadata batch is acknowledged.
	for len(tracker.metadata) > 0 {
		n := min(256, len(tracker.metadata))
		if err := s.sendPage(ctx, r, tracker, session, tracker.metadata[:n], 1); err != nil {
			return err
		}
		tracker.metadata = tracker.metadata[n:]
	}
	return nil
}

func (s *Store) sendFileLane(ctx context.Context, r *Remote, tracker *uploadTracker, session string, lane, workers, rawWindow int) error {
	var small []Offer
	bytes := 0
	flush := func() error {
		if len(small) == 0 {
			return nil
		}
		err := s.sendPage(ctx, r, tracker, session, small, 1)
		small = small[:0]
		bytes = 0
		return err
	}
	for i := lane; i < len(tracker.files); i += workers {
		if err := ctx.Err(); err != nil {
			return err
		}
		file := tracker.files[i]
		if len(file) == 1 && file[0].Size < 1<<20 {
			if len(small) == 256 || bytes+file[0].Size > 4<<20 {
				if err := flush(); err != nil {
					return err
				}
			}
			small = append(small, file[0])
			bytes += file[0].Size
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		// A raw PUT window always belongs to a single logical file.
		for len(file) > 0 {
			n := min(len(file), 256)
			if err := s.sendPage(ctx, r, tracker, session, file[:n], rawWindow); err != nil {
				return err
			}
			file = file[n:]
		}
	}
	return flush()
}

func (s *Store) sendPage(ctx context.Context, r *Remote, tracker *uploadTracker, session string, offers []Offer, rawWindow int) error {
	path := "/uploads/" + session
	var result struct {
		Missing []objectID `json:"missing"`
	}
	if e := r.call(ctx, "POST", path+"/missing", nil, map[string]any{"objects": offers}, &result); e != nil {
		return e
	}
	offered := map[objectID]int{}
	for _, o := range offers {
		offered[objectID{o.Kind, o.Digest}] = o.Size
	}
	missing := map[objectID]bool{}
	for _, o := range result.Missing {
		if _, ok := offered[o]; !ok || missing[o] {
			return errors.New("服务请求不在协商范围内的对象")
		}
		missing[o] = true
	}
	for o := range offered {
		if !missing[o] {
			tracker.confirm(o)
		}
	}
	var large, small []Offer
	for _, id := range result.Missing {
		o := Offer{Kind: id.Kind, Digest: id.Digest, Size: offered[id]}
		if o.Size >= 1<<20 {
			large = append(large, o)
		} else {
			small = append(small, o)
		}
	}
	if err := s.sendLarge(ctx, r, tracker, path, large, rawWindow); err != nil {
		return err
	}
	batch := []uploadItem{}
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var ack struct {
			Items []struct {
				Object objectID `json:"object"`
				Status string   `json:"status"`
			} `json:"items"`
		}
		body, e := json.Marshal(map[string]any{"items": batch})
		if e != nil {
			return e
		}
		observe, e := tracker.batchObserver(batch, body)
		if e != nil {
			return e
		}
		if e := r.callBytes(ctx, "POST", path+"/batch", nil, body, "application/json", &ack, observe); e != nil {
			return e
		}
		if len(ack.Items) != len(batch) {
			return errors.New("NAS 对象确认数量不一致")
		}
		for i, a := range ack.Items {
			if a.Object.Kind != batch[i].Kind || a.Object.Digest != batch[i].Digest || a.Status != "stored" && a.Status != "exists" {
				return fmt.Errorf("对象保存未确认 %s (%s)", batch[i].Digest, a.Status)
			}
			tracker.confirm(a.Object)
		}
		batch = nil
		size = 0
		return nil
	}
	for _, o := range small {
		sz := o.Size
		data, e := s.load(objectfmt.Ref{Kind: o.Kind, Digest: o.Digest})
		if e != nil {
			return e
		}
		if size+sz > 4<<20 {
			if e = flush(); e != nil {
				return e
			}
		}
		batch = append(batch, uploadItem{o.Kind, o.Digest, data})
		size += sz
	}
	return flush()
}

const maxRawUploads = 4

// Keep a small window of raw PUTs in flight so checksum, network and NAS
// durable writes overlap. Only active workers load blocks into memory.
func (s *Store) sendLarge(ctx context.Context, r *Remote, tracker *uploadTracker, path string, objects []Offer, rawWindow int) error {
	if len(objects) == 0 {
		return nil
	}
	workers := min(rawWindow, len(objects))
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var group sync.WaitGroup
	for worker := range workers {
		group.Go(func() {
			for i := worker; i < len(objects) && ctx.Err() == nil; i += workers {
				o := objects[i]
				id := objectID{o.Kind, o.Digest}
				data, err := s.load(objectfmt.Ref{Kind: o.Kind, Digest: o.Digest})
				if err == nil {
					err = r.callBytes(ctx, "PUT", path+"/objects/"+o.Digest, url.Values{"kind": {o.Kind}}, data, "application/octet-stream", nil, tracker.rawObserver(id, o.Size))
				}
				if err != nil {
					cancel(err)
					return
				}
				tracker.confirm(id)
			}
		})
	}
	group.Wait()
	return context.Cause(ctx)
}
