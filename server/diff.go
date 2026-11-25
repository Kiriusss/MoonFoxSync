package server

import (
	"context"
	"net/http"
	"sort"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

type change struct {
	Path     string `json:"path"`
	Status   string `json:"status"`
	OldPath  string `json:"oldPath,omitempty"`
	identity string
}

// Equal subtree hashes allow unchanged directory contents to be skipped.
// New or removed directories carry their subtree as a single change.
func compareTrees(ctx context.Context, from, to string, load objectfmt.Load) ([]change, error) {
	changes := []change{}
	const maxReported = 1 << 20
	var walk func(string, string, string, int) error
	walk = func(prefix, left, right string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if left == right {
			return nil
		}
		if depth > 256 {
			return large()
		}
		old, e := objectfmt.ReadDir(left, load)
		if e != nil {
			return e
		}
		newer, e := objectfmt.ReadDir(right, load)
		if e != nil {
			return e
		}
		i, j := 0, 0
		for i < len(old) || j < len(newer) {
			if len(changes) >= maxReported {
				return large()
			}
			var item objectfmt.Entry
			status := ""
			switch {
			case j == len(newer) || (i < len(old) && old[i].Name < newer[j].Name):
				item = old[i]
				i++
				status = "deleted"
			case i == len(old) || newer[j].Name < old[i].Name:
				item = newer[j]
				j++
				status = "added"
			default:
				before, after := old[i], newer[j]
				i++
				j++
				item = after
				if before == after {
					continue
				}
				if before.Kind == "directory" && after.Kind == "directory" {
					if e = walk(prefix+item.Name+"/", before.Digest, after.Digest, depth+1); e != nil {
						return e
					}
					if before.MtimeNS == after.MtimeNS {
						continue
					}
				}
				status = "modified"
			}
			key := item.Kind + ":" + item.Digest
			if item.Kind == "symlink" {
				key = "symlink:" + item.Target
			}
			changes = append(changes, change{Path: prefix + item.Name, Status: status, identity: key})
		}
		return nil
	}
	if e := walk("", from, to, 0); e != nil {
		return nil, e
	}
	// Infer a move only for an unambiguous deleted/added pair.
	added, removed := map[string][]int{}, map[string][]int{}
	for i, c := range changes {
		switch c.Status {
		case "added":
			added[c.identity] = append(added[c.identity], i)
		case "deleted":
			removed[c.identity] = append(removed[c.identity], i)
		}
	}
	for key, ins := range added {
		outs := removed[key]
		if len(ins) == 1 && len(outs) == 1 {
			a, b := ins[0], outs[0]
			changes[a].Status = "moved"
			changes[a].OldPath = changes[b].Path
			changes[b].Status = ""
		}
	}
	out := changes[:0]
	for _, c := range changes {
		if c.Status != "" {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
func (s *Store) diff(w http.ResponseWriter, r *http.Request, dev string) error {
	unit := r.PathValue("unitId")
	if e := s.access(unit, dev, false); e != nil {
		return e
	}
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	a, e := s.lookup(unit, from, "")
	if e != nil {
		return e
	}
	b, e := s.lookup(unit, to, "")
	if e != nil {
		return e
	}
	scope := dev + ":diff:" + unit + ":" + from + ":" + to
	lim, start, e := page(r, scope)
	if e != nil {
		return e
	}
	changes, e := compareTrees(r.Context(), a.Digest, b.Digest, s.read)
	if e != nil {
		return e
	}
	if start > len(changes) {
		return bad("invalid diff offset")
	}
	end := min(len(changes), start+lim)
	return reply(w, 200, fields{"items": changes[start:end], "nextCursor": next(scope, start, lim, len(changes)-start)})
}
