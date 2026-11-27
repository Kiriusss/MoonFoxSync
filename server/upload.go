package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

const blockMax = 4 << 20

type asset struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	FileDigest string `json:"fileDigest"`
}
type draft struct {
	BaseContentDigest string            `json:"baseContentDigest,omitempty"`
	RequestID         string            `json:"requestId"`
	ID                string            `json:"clientSnapshotId"`
	Parent            *string           `json:"parentSnapshotId"`
	Base              *string           `json:"expectedHead"`
	Root              string            `json:"rootDigest"`
	Revision          int64             `json:"scopeRevision"`
	Captured          string            `json:"capturedAt"`
	Label             string            `json:"label"`
	Checkpoint        bool              `json:"checkpoint"`
	GitHead           string            `json:"gitHead,omitempty"`
	GitBranch         string            `json:"gitBranch,omitempty"`
	Resolved          string            `json:"resolvedForkSnapshotId,omitempty"`
	Links             []json.RawMessage `json:"links"`
	Attachments       []asset           `json:"attachments"`
}

var assetName = regexp.MustCompile("^[a-zA-Z0-9_-][a-zA-Z0-9._-]{0,254}$")

func same(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func (d draft) validate() error {
	if d.BaseContentDigest != "" && (d.Base == nil || !validDigest.MatchString(d.BaseContentDigest)) {
		return bad("invalid content baseline")
	}
	if !validID.MatchString(d.RequestID) || !validID.MatchString(d.ID) || !validDigest.MatchString(d.Root) || !same(d.Parent, d.Base) || (d.Parent != nil && (!validID.MatchString(*d.Parent) || *d.Parent == d.ID)) || d.Revision < 1 || d.Revision > 9007199254740991 || len([]rune(d.Label)) > 500 || d.Links == nil || d.Attachments == nil {
		return bad("invalid snapshot fields")
	}
	if _, e := time.Parse(time.RFC3339Nano, d.Captured); e != nil {
		return bad("invalid capturedAt")
	}
	if len(d.Links) != 0 {
		return bad("linked units are not supported by this release")
	}
	if d.Resolved != "" {
		return bad("fork resolution metadata is not supported by this release")
	}
	names := map[string]bool{}
	for _, a := range d.Attachments {
		if !assetName.MatchString(a.Name) || a.Kind != "git-bundle" || !validDigest.MatchString(a.FileDigest) || names[a.Name] {
			return bad("invalid attachment")
		}
		names[a.Name] = true
	}
	return nil
}

type session struct {
	id, unit, device string
	draft            draft
	raw, receipt     []byte
	expires          int64
}

func (s *Store) loadSession(id, dev string) (session, error) {
	var a session
	a.id = id
	e := s.db.QueryRow("SELECT unit,device,draft,receipt,expires FROM uploads WHERE id=? AND device=?", id, dev).Scan(&a.unit, &a.device, &a.raw, &a.receipt, &a.expires)
	if e != nil {
		return a, e
	}
	if e = s.access(a.unit, dev, true); e != nil {
		return a, e
	}
	if len(a.receipt) == 0 && a.expires <= time.Now().Unix() {
		return a, &problem{410, "SESSION_EXPIRED", "session expired", "restart_session", nil}
	}
	e = json.Unmarshal(a.raw, &a.draft)
	return a, e
}
func (a session) representation() fields {
	var expiry, receipt any
	state := "open"
	if len(a.receipt) > 0 {
		receipt = json.RawMessage(a.receipt)
		state = "finalized"
	} else {
		expiry = stamp(time.Unix(a.expires, 0))
	}
	return fields{"uploadId": a.id, "unitId": a.unit, "clientSnapshotId": a.draft.ID, "receipt": receipt, "state": state, "expiresAt": expiry}
}
func (s *Store) begin(w http.ResponseWriter, r *http.Request, dev string) error {
	unit := r.PathValue("unitId")
	if e := s.access(unit, dev, true); e != nil {
		return e
	}
	var d draft
	if e := decode(r, &d, 1<<20); e != nil {
		return e
	}
	if e := d.validate(); e != nil {
		return e
	}
	raw, e := json.Marshal(d)
	if e != nil {
		return e
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var id, own, existingUnit string
	var old, receipt []byte
	var expiry int64
	e = tx.QueryRow("SELECT id,device,unit,draft,expires,receipt FROM uploads WHERE (device=? AND request=?) OR (unit=? AND snapshot=?)", dev, d.RequestID, unit, d.ID).Scan(&id, &own, &existingUnit, &old, &expiry, &receipt)
	status := 200
	if errors.Is(e, sql.ErrNoRows) {
		status = 201
		id = newID()
		expiry = time.Now().Add(7 * 24 * time.Hour).Unix()
		_, e = tx.Exec("INSERT INTO uploads(id,unit,device,request,snapshot,draft,expires) VALUES(?,?,?,?,?,?,?)", id, unit, dev, d.RequestID, d.ID, raw, expiry)
	} else if e == nil {
		if own != dev || existingUnit != unit {
			return conflict("snapshot or request identity already used")
		}
		if !bytes.Equal(old, raw) {
			var frozen draft
			if e = json.Unmarshal(old, &frozen); e != nil {
				return e
			}
			frozen.RequestID = d.RequestID
			compare, _ := json.Marshal(frozen)
			if !bytes.Equal(raw, compare) || (expiry > time.Now().Unix() && len(receipt) == 0) {
				return conflict("snapshot or request parameters changed")
			}
			if len(receipt) == 0 {
				expiry = time.Now().Add(7 * 24 * time.Hour).Unix()
				_, e = tx.Exec("UPDATE uploads SET request=?,draft=?,expires=? WHERE id=?", d.RequestID, raw, expiry, id)
			}
		} else if len(receipt) == 0 && expiry <= time.Now().Unix() {
			return &problem{410, "SESSION_EXPIRED", "session expired", "restart_session", nil}
		}
	}
	if e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return reply(w, status, (session{id: id, unit: unit, draft: d, expires: expiry, receipt: receipt}).representation())
}
func (s *Store) session(w http.ResponseWriter, r *http.Request, dev string) error {
	a, e := s.loadSession(r.PathValue("uploadId"), dev)
	if e != nil {
		return e
	}
	return reply(w, 200, a.representation())
}
func (s *Store) active(r *http.Request, dev string) (session, error) {
	a, e := s.loadSession(r.PathValue("uploadId"), dev)
	if e == nil && len(a.receipt) > 0 {
		e = conflict("session finalized")
	}
	return a, e
}

type offer struct {
	objectfmt.Ref
	Size int64 `json:"size"`
}

func goodRef(o objectfmt.Ref) bool {
	return validDigest.MatchString(o.Digest) && (o.Kind == "tree" || o.Kind == "file" || o.Kind == "chunk")
}

// For this lookup the snapshot is unknown. SQLite can otherwise choose the
// covering (unit,snapshot,kind,digest) primary key and scan every ref in a unit
// per digest. The existing reusable index resolves all three known keys.
const reusableRefSQL = `SELECT 1 FROM refs r INDEXED BY reusable
    WHERE r.unit=? AND r.kind=? AND r.digest=? AND NOT EXISTS(
    SELECT 1 FROM snapshot_tombstones t WHERE t.unit=r.unit AND t.snapshot=r.snapshot)`

func (s *Store) missing(w http.ResponseWriter, r *http.Request, dev string) error {
	a, e := s.active(r, dev)
	if e != nil {
		return e
	}
	var body struct{ Objects []offer }
	if e = decode(r, &body, 1<<20); e != nil {
		return e
	}
	if n := len(body.Objects); n < 1 || n > 256 {
		return bad("offer count out of bounds")
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	out := []objectfmt.Ref{}
	for _, o := range body.Objects {
		if !goodRef(o.Ref) || o.Size < 0 || o.Size > blockMax {
			return bad("invalid offer")
		}
		_, e = tx.Exec("INSERT OR IGNORE INTO offers(upload,kind,digest,size,ready) SELECT ?,?,?,?,EXISTS("+reusableRefSQL+")", a.id, o.Kind, o.Digest, o.Size, a.unit, o.Kind, o.Digest)
		if e != nil {
			return e
		}
		var sz int64
		var ready bool
		e = tx.QueryRow("SELECT size,ready=2 OR EXISTS ("+reusableRefSQL+") FROM offers WHERE upload=? AND kind=? AND digest=?", a.unit, o.Kind, o.Digest, a.id, o.Kind, o.Digest).Scan(&sz, &ready)
		if e != nil {
			return e
		}
		if sz != o.Size {
			return conflict("object offer length changed")
		}
		if !ready {
			out = append(out, o.Ref)
		}
	}
	if _, e = tx.Exec("UPDATE uploads SET expires=? WHERE id=?", time.Now().Add(7*24*time.Hour).Unix(), a.id); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return reply(w, 200, fields{"missing": out})
}
func (s *Store) path(o objectfmt.Ref) string {
	return filepath.Join(s.dir, "objects", o.Kind, o.Digest[:2], o.Digest)
}
func (s *Store) read(o objectfmt.Ref) ([]byte, error) {
	if !goodRef(o) {
		return nil, invalid("invalid object key")
	}
	b, e := os.ReadFile(s.path(o))
	if e != nil {
		return nil, e
	}
	if len(b) > blockMax || hash(b) != o.Digest {
		return nil, errors.New("corrupt object on disk")
	}
	return b, nil
}

// Object shard directories live for the lifetime of the store. Remember a
// directory only after its parent has been synced, including on EEXIST where
// another writer might still be publishing a newly created directory.
func (s *Store) ensureObjectDir(dir string) error {
	if _, ok := s.durableDirs.Load(dir); ok {
		return nil
	}
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	s.durableDirs.Store(dir, struct{}{})
	return nil
}
func (s *Store) install(o objectfmt.Ref, b []byte) error {
	dest := s.path(o)
	if _, e := os.Stat(dest); e == nil {
		if _, e = s.read(o); e != nil {
			return e
		}
		// A concurrent installer may have linked the file just before we
		// saw it. Sync its name before acknowledging our own offer.
		return syncDir(filepath.Dir(dest))
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if e := s.ensureObjectDir(filepath.Dir(filepath.Dir(dest))); e != nil {
		return e
	}
	if e := s.ensureObjectDir(filepath.Dir(dest)); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Join(s.dir, "incoming"), "object-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	c := f.Close()
	if e == nil {
		e = c
	}
	if e != nil {
		return e
	}
	if e = os.Link(tmp, dest); e != nil {
		if !errors.Is(e, os.ErrExist) {
			return e
		}
		if _, e = s.read(o); e != nil {
			return e
		}
	}
	return syncDir(filepath.Dir(dest))
}

// The caller holds objectUse until installed objects have durable offer
// rows. A batch shares that transaction across all successful objects.
func (s *Store) stageObject(a session, o objectfmt.Ref, b []byte) (string, error) {
	if !goodRef(o) || hash(b) != o.Digest {
		return "invalid", invalid("object digest mismatch")
	}
	var size int
	var ready bool
	e := s.db.QueryRow("SELECT size,ready<>0 FROM offers WHERE upload=? AND kind=? AND digest=?", a.id, o.Kind, o.Digest).Scan(&size, &ready)
	if errors.Is(e, sql.ErrNoRows) {
		return "invalid", bad("offer object before upload")
	}
	if e != nil {
		return "transient", e
	}
	if size != len(b) || size > blockMax {
		return "invalid", invalid("offered size differs")
	}
	if e = s.install(o, b); e != nil {
		return "transient", e
	}
	if ready {
		return "exists", nil
	}
	return "stored", nil
}
func (s *Store) put(w http.ResponseWriter, r *http.Request, dev string) error {
	a, e := s.active(r, dev)
	if e != nil {
		return e
	}
	if r.ContentLength > blockMax {
		return large()
	}
	var b []byte
	if r.ContentLength >= 0 {
		b = make([]byte, int(r.ContentLength))
		_, e = io.ReadFull(r.Body, b)
	} else {
		b, e = io.ReadAll(io.LimitReader(r.Body, blockMax+1))
	}
	if e != nil {
		return e
	}
	if len(b) > blockMax {
		return large()
	}
	s.objectUse.RLock()
	defer s.objectUse.RUnlock()
	o := objectfmt.Ref{Kind: r.URL.Query().Get("kind"), Digest: r.PathValue("digest")}
	if _, e = s.stageObject(a, o, b); e != nil {
		return e
	}
	if _, e = s.db.ExecContext(r.Context(), "UPDATE offers SET ready=2 WHERE upload=? AND kind=? AND digest=?", a.id, o.Kind, o.Digest); e != nil {
		return e
	}
	w.WriteHeader(204)
	return nil
}
func (s *Store) batch(w http.ResponseWriter, r *http.Request, dev string) error {
	a, e := s.active(r, dev)
	if e != nil {
		return e
	}
	var body struct {
		Items []struct {
			objectfmt.Ref
			Data []byte `json:"data"`
		}
	}
	if e = decode(r, &body, 6<<20); e != nil {
		return e
	}
	if n := len(body.Items); n == 0 || n > 256 {
		return bad("batch count out of bounds")
	}
	total := 0
	for _, o := range body.Items {
		total += len(o.Data)
	}
	if total > blockMax {
		return large()
	}
	// Keep filesystem I/O outside the SQLite write transaction. Up to four
	// NAS disk workers overlap file syncs, shared across all concurrent batches.
	s.objectUse.RLock()
	defer s.objectUse.RUnlock()
	type staged struct {
		status string
		err    error
	}
	results := make([]staged, len(body.Items))
	var group sync.WaitGroup
	workers := min(len(body.Items), cap(s.batchDiskSlots))
	for worker := range workers {
		group.Go(func() {
			select {
			case s.batchDiskSlots <- struct{}{}:
				defer func() { <-s.batchDiskSlots }()
			case <-r.Context().Done():
				return
			}
			for i := worker; i < len(body.Items) && r.Context().Err() == nil; i += workers {
				o := body.Items[i]
				results[i].status, results[i].err = s.stageObject(a, o.Ref, o.Data)
				if results[i].status == "transient" {
					return
				}
			}
		})
	}
	group.Wait()
	if err := r.Context().Err(); err != nil {
		return err
	}
	for _, result := range results {
		if result.status == "transient" {
			return result.err
		}
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	mark, e := tx.Prepare("UPDATE offers SET ready=2 WHERE upload=? AND kind=? AND digest=?")
	if e != nil {
		return e
	}
	defer mark.Close()
	out := make([]fields, 0, len(body.Items))
	confirmed := make(map[objectfmt.Ref]bool)
	for i, result := range results {
		o := body.Items[i].Ref
		status := result.status
		if result.err == nil {
			if confirmed[o] {
				status = "exists"
			} else {
				if _, e = mark.Exec(a.id, o.Kind, o.Digest); e != nil {
					return e
				}
				confirmed[o] = true
			}
		}
		out = append(out, fields{"object": o, "status": status})
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return reply(w, 200, fields{"items": out})
}
func (s *Store) finalize(w http.ResponseWriter, r *http.Request, dev string) error {
	a, e := s.loadSession(r.PathValue("uploadId"), dev)
	if e != nil {
		return e
	}
	if len(a.receipt) > 0 {
		return reply(w, 200, json.RawMessage(a.receipt))
	}
	var deleteEpoch int64
	const epochSQL = "SELECT coalesce(max(rowid),0) FROM snapshot_tombstones WHERE unit=?"
	if e = s.db.QueryRowContext(r.Context(), epochSQL, a.unit).Scan(&deleteEpoch); e != nil {
		return e
	}
	// A ready offer belongs to this session. Reusable references belong to a
	// visible snapshot in this unit. Prefetch the common case without loading
	// the unit's entire history or letting unrelated offers grow memory unbounded.
	// Any historical or unprefetched object takes the original indexed lookup.
	authorized, e := s.prefetchPublishRefs(r.Context(), a)
	if e != nil {
		return e
	}
	seen := map[objectfmt.Ref]bool{}
	var loadErr error
	load := func(o objectfmt.Ref) ([]byte, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		if !authorized[o] && !seen[o] {
			var ok bool
			err := s.db.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM offers WHERE upload=? AND kind=? AND digest=? AND ready=2) OR EXISTS("+reusableRefSQL+")", a.id, o.Kind, o.Digest, a.unit, o.Kind, o.Digest).Scan(&ok)
			if err != nil {
				loadErr = err
				return nil, err
			}
			if !ok {
				loadErr = &problem{409, "OBJECTS_MISSING", "required object not confirmed", "upload_missing", []objectfmt.Ref{o}}
				return nil, loadErr
			}
		}
		// VerifyTree/VerifyFile validate each ref and hash the supplied bytes
		// before decoding or accepting them. Store.read would hash them twice.
		b, err := os.ReadFile(s.path(o))
		if err != nil {
			loadErr = err
			return nil, err
		}
		seen[o] = true
		return b, nil
	}
	d := a.draft
	e = objectfmt.VerifyTree(d.Root, load)
	if e == nil {
		for _, att := range d.Attachments {
			if e = objectfmt.VerifyFile(att.FileDigest, load); e != nil {
				break
			}
		}
	}
	if loadErr != nil {
		return loadErr
	}
	if errors.Is(e, objectfmt.ErrObjectIntegrity) {
		return errors.New("corrupt object on disk")
	}
	if e != nil {
		return invalid(fmt.Sprintf("invalid snapshot closure: %v", e))
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var head *string
	var role string
	e = tx.QueryRow("SELECT u.head,g.role FROM units u JOIN effective_grants g ON g.unit=u.id JOIN devices d ON d.id=g.device WHERE u.id=? AND d.id=? AND d.revoked=0", a.unit, dev).Scan(&head, &role)
	if e != nil {
		return e
	}
	if role != "sync" {
		return forbidden()
	}
	var existing []byte
	if e = tx.QueryRow("SELECT receipt FROM uploads WHERE id=?", a.id).Scan(&existing); e != nil {
		return e
	}
	if len(existing) > 0 {
		tx.Rollback()
		return reply(w, 200, json.RawMessage(existing))
	}
	var currentEpoch int64
	if e = tx.QueryRowContext(r.Context(), epochSQL, a.unit).Scan(&currentEpoch); e != nil {
		return e
	}
	if deleteEpoch != currentEpoch {
		return conflict("可读历史已变化，请重试上传")
	}
	if d.Parent != nil {
		var exists bool
		if e = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM snapshots WHERE unit=? AND id=?)", a.unit, *d.Parent).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return &problem{409, "PARENT_MISSING", "publish parent first", "upload_parent", nil}
		}
	}
	outcome := "fork"
	advance := same(head, d.Base)
	if !advance && head != nil && d.Base != nil {
		advance, e = collapsedBaseMatches(r.Context(), tx, a.unit, *d.Base, *head)
		if e != nil {
			return e
		}
	}
	if advance {
		advance, e = s.mayAdvanceShared(r.Context(), tx, a.unit, dev, d)
		if e != nil {
			return e
		}
	}
	if advance {
		outcome = "published"
		head = &d.ID
		if _, e = tx.Exec("UPDATE units SET head=? WHERE id=?", d.ID, a.unit); e != nil {
			return e
		}
	}
	now := stamp(time.Now())
	rec := fields{"unitId": a.unit, "snapshotId": d.ID, "rootDigest": d.Root, "outcome": outcome, "headAtCommit": head, "committedAt": now}
	rb, _ := json.Marshal(rec)
	var snapshot fields
	_ = json.Unmarshal(a.raw, &snapshot)
	for _, k := range []string{"requestId", "clientSnapshotId", "expectedHead", "baseContentDigest"} {
		delete(snapshot, k)
	}
	for k, v := range (fields{"snapshotId": d.ID, "unitId": a.unit, "deviceId": dev, "committedAt": now, "outcome": outcome}) {
		snapshot[k] = v
	}
	sb, _ := json.Marshal(snapshot)
	if _, e = tx.Exec("INSERT INTO snapshots(unit,id,root,record) VALUES(?,?,?,?)", a.unit, d.ID, d.Root, sb); e != nil {
		return e
	}
	if e = writePublishRefs(r.Context(), tx, a.unit, d.ID, seen); e != nil {
		return e
	}
	if e = s.projectNewHistory(r.Context(), tx, a.unit, d.ID); e != nil {
		return e
	}
	if e = s.applyPolicy(r.Context(), tx, a.unit, time.Now()); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE uploads SET receipt=? WHERE id=?", rb, a.id); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return reply(w, 200, rec)
}

func (s *Store) prefetchPublishRefs(ctx context.Context, a session) (map[objectfmt.Ref]bool, error) {
	const limit = 65536
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (SELECT kind,digest FROM offers WHERE upload=? AND ready=2 LIMIT ?)
        UNION ALL SELECT * FROM (SELECT kind,digest FROM visible_refs WHERE unit=? AND snapshot=? LIMIT ?)`, a.id, limit, a.unit, a.draft.Parent, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	allowed := make(map[objectfmt.Ref]bool)
	for rows.Next() {
		var ref objectfmt.Ref
		if err := rows.Scan(&ref.Kind, &ref.Digest); err != nil {
			return nil, err
		}
		allowed[ref] = true
	}
	return allowed, rows.Err()
}

// A multi-row insert amortizes SQLite parsing and statement execution, while
// the snapshot, all of its references and its receipt still commit atomically.
func writePublishRefs(ctx context.Context, tx *sql.Tx, unit, snapshot string, seen map[objectfmt.Ref]bool) error {
	const batch = 256
	args := make([]any, 2, 2+batch*2)
	args[0], args[1] = unit, snapshot
	flush := func() error {
		n := (len(args) - 2) / 2
		sql := "INSERT INTO refs(unit,snapshot,kind,digest) VALUES " + strings.TrimSuffix(strings.Repeat("(?1,?2,?,?),", n), ",")
		_, err := tx.ExecContext(ctx, sql, args...)
		args = args[:2]
		return err
	}
	for ref := range seen {
		args = append(args, ref.Kind, ref.Digest)
		if len(args) == cap(args) {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if len(args) > 2 {
		return flush()
	}
	return nil
}
