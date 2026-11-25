package server

import (
	"bytes"
	"errors"
	"kirius.local/kiriusdrive/internal/objectfmt"
	"net/http"
	"unicode/utf8"
)

func (s *Store) mergeMemberPreview(w http.ResponseWriter, r *http.Request, dev string) error {
	unit := r.PathValue("unitId")
	if err := s.access(unit, dev, false); err != nil {
		return err
	}
	snap, p := r.URL.Query().Get("snapshotId"), r.URL.Query().Get("path")
	if p == "" {
		return bad("choose a member path")
	}
	w.Header().Set("Cache-Control", "no-store")
	entry, err := s.lookup(unit, snap, p)
	if err != nil {
		// A missing snapshot is sql.ErrNoRows. Only a missing member is previewable.
		var prob *problem
		if errors.As(err, &prob) && prob.Status == 404 {
			return reply(w, 200, fields{"kind": "missing", "truncated": false})
		}
		return err
	}
	out := fields{"kind": entry.Kind, "truncated": false}
	switch entry.Kind {
	case "symlink":
		out["target"] = entry.Target
	case "file":
		info, err := objectfmt.ReadFile(entry.Digest, s.read)
		if err != nil {
			return err
		}
		limit := min(info.Size, int64(mergeTextLimit))
		var buf bytes.Buffer
		if err = objectfmt.WriteRange(info, 0, limit, &buf, s.read); err != nil {
			return err
		}
		data := buf.Bytes()
		truncated := limit < info.Size
		if truncated && len(data) > 0 {
			start := len(data) - 1
			for start > 0 && len(data)-start < utf8.UTFMax && !utf8.RuneStart(data[start]) {
				start--
			}
			if !utf8.FullRune(data[start:]) {
				data = data[:start]
			}
		}
		out["size"], out["truncated"] = info.Size, truncated
		if utf8.Valid(data) && bytes.IndexByte(data, 0) < 0 {
			out["kind"], out["text"] = "text", string(data)
		} else {
			out["kind"] = "binary"
		}
	}
	return reply(w, 200, out)
}
