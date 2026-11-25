package server

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

func TestPublishRechecksReusedObjectsAndIndexesOnlyReachableRefs(t *testing.T) {
	f := timelineFixtureAt(t)
	original := []byte("old bytes")
	first := f.add("", string(original), "published")
	begin := func(parent, root string) (string, string) {
		t.Helper()
		d := draft{RequestID: newID(), ID: newID(), Parent: &parent, Base: &parent, Root: root, Captured: stamp(time.Now()), Revision: 1, Links: []json.RawMessage{}, Attachments: []asset{}}
		b, _ := f.request("POST", f.path+"/uploads", f.writer, d, 201)
		return d.ID, "/uploads/" + parse(t, b)["uploadId"].(string)
	}
	second, session := begin(first.id, first.root)
	chunk := objectfmt.Ref{Kind: "chunk", Digest: hash(original)}
	disk := f.s.path(chunk)
	if err := os.WriteFile(disk, []byte("bad bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	f.request("POST", session+"/finalize", f.writer, nil, 500)
	if err := os.Remove(disk); err != nil {
		t.Fatal(err)
	}
	f.request("POST", session+"/finalize", f.writer, nil, 500)
	f.request("GET", f.path+"/snapshots/"+second, f.writer, nil, 404)
	if err := os.WriteFile(disk, original, 0600); err != nil {
		t.Fatal(err)
	}
	f.request("POST", session+"/finalize", f.writer, nil, 200)

	// Neither unused parent refs nor an extra ready offer may be published.
	var batch, offers []fields
	add := func(ref objectfmt.Ref, raw []byte) error {
		batch = append(batch, fields{"kind": ref.Kind, "digest": ref.Digest, "data": raw})
		offers = append(offers, fields{"kind": ref.Kind, "digest": ref.Digest, "size": len(raw)})
		return nil
	}
	root, err := objectfmt.BuildDir(nil, add)
	if err != nil {
		t.Fatal(err)
	}
	extra := []byte("not in this snapshot")
	add(objectfmt.Ref{Kind: "chunk", Digest: hash(extra)}, extra)
	third, session := begin(second, root)
	f.request("POST", session+"/missing", f.writer, fields{"objects": offers}, 200)
	f.request("POST", session+"/batch", f.writer, fields{"items": batch}, 200)
	f.request("POST", session+"/finalize", f.writer, nil, 200)
	var refs int
	if err := f.s.db.QueryRow("SELECT count(*) FROM refs WHERE unit=? AND snapshot=?", f.unit, third).Scan(&refs); err != nil || refs != 1 {
		t.Fatalf("empty snapshot refs=%d err=%v", refs, err)
	}
	for _, ref := range []objectfmt.Ref{chunk, {Kind: "chunk", Digest: hash(extra)}} {
		f.request("GET", f.path+"/snapshots/"+third+"/objects/"+ref.Digest+"?kind="+ref.Kind, f.writer, nil, 404)
	}
}
