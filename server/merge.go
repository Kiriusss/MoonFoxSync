package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"kirius.local/kiriusdrive/internal/objectfmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const schemaMerge = `
CREATE TABLE IF NOT EXISTS merge_requests(unit TEXT NOT NULL REFERENCES units(id),device TEXT NOT NULL REFERENCES devices(id),request TEXT NOT NULL,bodyhash TEXT NOT NULL,receipt BLOB NOT NULL,PRIMARY KEY(unit,device,request));
PRAGMA user_version=5;
`

type mergeInput struct {
	Main    string        `json:"mainHead"`
	Branch  string        `json:"branchSnapshotId"`
	Request string        `json:"requestId"`
	Label   string        `json:"label"`
	Choices []mergeChoice `json:"choices"`
}
type versionNode struct {
	Seq           int64
	Root, Outcome string
	Parent, Other *string
	Attachments   []asset
}

func (s *Store) prepareMerge(ctx context.Context, unit, branch string) (*merger, versionNode, versionNode, versionNode, error) {
	m := &merger{ctx: ctx, s: s, created: make(map[objectfmt.Ref][]byte), normalized: make(map[string]string), plan: mergePlan{Unit: unit, Branch: branch, Conflicts: []mergeConflict{}, Auto: []string{}}}
	var a, z, b versionNode
	var head *string
	if !validID.MatchString(branch) {
		return m, a, z, b, bad("invalid branch snapshot")
	}
	if e := s.db.QueryRowContext(ctx, "SELECT head FROM units WHERE id=?", unit).Scan(&head); e != nil {
		return m, a, z, b, e
	}
	var visible string
	if e := s.db.QueryRowContext(ctx, "SELECT id FROM visible_snapshots WHERE unit=? AND id=?", unit, branch).Scan(&visible); e != nil {
		return m, a, z, b, e
	}
	if head == nil {
		return m, a, z, b, conflict("资料库还没有主线版本")
	}
	m.plan.Main = *head
	nodes := map[string]versionNode{}
	load := func(id string) (versionNode, error) {
		if v, ok := nodes[id]; ok {
			return v, nil
		}
		var v versionNode
		var attachments []byte
		e := s.db.QueryRowContext(ctx, `SELECT seq,root,json_extract(record,'$.outcome'),json_extract(record,'$.parentSnapshotId'),json_extract(record,'$.mergeParentSnapshotId'),COALESCE(json_extract(record,'$.attachments'),'[]') FROM snapshots WHERE unit=? AND id=?`, unit, id).Scan(&v.Seq, &v.Root, &v.Outcome, &v.Parent, &v.Other, &attachments)
		if e == nil {
			e = json.Unmarshal(attachments, &v.Attachments)
		}
		if e == nil {
			nodes[id] = v
		}
		return v, e
	}
	var e error
	if a, e = load(*head); e != nil {
		return m, a, z, b, e
	}
	if z, e = load(branch); e != nil {
		return m, a, z, b, e
	}
	if z.Outcome != "fork" {
		return m, a, z, b, bad("需要选择此资料库的分叉快照")
	}
	ancestors := func(tip string) (map[string]bool, error) {
		found := map[string]bool{}
		stack := []string{tip}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if found[id] {
				continue
			}
			found[id] = true
			v, err := load(id)
			if err != nil {
				return nil, err
			}
			for _, p := range []*string{v.Parent, v.Other} {
				if p != nil {
					stack = append(stack, *p)
				}
			}
		}
		return found, nil
	}
	mainHistory, e := ancestors(*head)
	if e != nil {
		return m, a, z, b, e
	}
	if mainHistory[branch] {
		m.plan.Already = true
		m.plan.Base = branch
		return m, a, z, z, nil
	}
	branchHistory, e := ancestors(branch)
	if e != nil {
		return m, a, z, b, e
	}
	// Shared ancestor with the latest topological sequence is a maximal base.
	// Criss-cross histories can have two maximal bases. In that case require
	// explicit resolution of every difference instead of silently picking a side.
	covered := map[string]bool{}
	for id := range branchHistory {
		if !mainHistory[id] {
			continue
		}
		v := nodes[id]
		if v.Seq > b.Seq {
			b = v
			m.plan.Base = id
		}
		for _, p := range []*string{v.Parent, v.Other} {
			if p != nil {
				covered[*p] = true
			}
		}
	}
	for id := range branchHistory {
		if mainHistory[id] && !covered[id] && id != m.plan.Base {
			m.plan.Ambiguous = true
			break
		}
	}
	if m.plan.Base != "" {
		if e := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM snapshot_tombstones WHERE unit=? AND snapshot=?)", unit, m.plan.Base).Scan(&m.plan.BaseHidden); e != nil {
			return m, a, z, b, e
		}
	}
	return m, a, z, b, nil
}
func (s *Store) mergePreview(w http.ResponseWriter, r *http.Request, d string) error {
	unit := r.PathValue("unitId")
	if err := s.access(unit, d, false); err != nil {
		return err
	}
	var input struct {
		Branch string `json:"branchSnapshotId"`
	}
	if err := decode(r, &input, 4096); err != nil {
		return err
	}
	m, a, z, b, err := s.prepareMerge(r.Context(), unit, input.Branch)
	if err != nil {
		return err
	}
	if !m.plan.Already {
		if _, err = m.directory("", b.Root, a.Root, z.Root, 0); err != nil {
			return err
		}
	}
	return reply(w, 200, m.plan)
}

type rowQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func mergeReplay(ctx context.Context, q rowQuery, unit, dev, req, body string) ([]byte, error) {
	var stored string
	var rec []byte
	e := q.QueryRowContext(ctx, "SELECT bodyhash,receipt FROM merge_requests WHERE unit=? AND device=? AND request=?", unit, dev, req).Scan(&stored, &rec)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e == nil && stored != body {
		return nil, conflict("该请求编号已用于不同的合并选择")
	}
	return rec, e
}

// Keep both parents' Git bundle attachment data accessible. If equal names
// refer to different objects, give the branch attachment an unused safe name.
func joinedAssets(main, branch []asset, branchID string) []asset {
	out := append([]asset{}, main...)
	taken := map[string]bool{}
	reserved := map[string]bool{}
	for _, v := range append(append([]asset{}, main...), branch...) {
		reserved[strings.ToLower(v.Name)] = true
	}
	for _, v := range main {
		taken[v.Name] = true
	}
	for _, v := range branch {
		if taken[v.Name] {
			duplicate := false
			for _, item := range main {
				if v == item {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			// Attachment identifiers use the ASCII protocol grammar.
			v.Name = altAssetName(v.Name, branchID, reserved)
		}
		taken[v.Name] = true
		out = append(out, v)
	}
	return out
}
func altAssetName(name, branch string, used map[string]bool) string {
	for i := 1; ; i++ {
		suffix := fmt.Sprintf("-fork-%s-%d", branch[len(branch)-8:], i)
		n := name
		if len(n)+len(suffix) > 255 {
			n = n[:255-len(suffix)]
		}
		candidate := n + suffix
		key := strings.ToLower(candidate)
		if !used[key] {
			used[key] = true
			return candidate
		}
	}
}
func (s *Store) mergeCommit(w http.ResponseWriter, r *http.Request, dev string) error {
	s.objectUse.RLock()
	defer s.objectUse.RUnlock()
	ctx := r.Context()
	unit := r.PathValue("unitId")
	if e := s.access(unit, dev, true); e != nil {
		return e
	}
	var in mergeInput
	if e := decode(r, &in, 4<<20); e != nil {
		return e
	}
	if !validID.MatchString(in.Request) || !validID.MatchString(in.Main) || !validID.MatchString(in.Branch) || len([]rune(in.Label)) > 500 {
		return bad("invalid merge identity or label")
	}
	if in.Choices == nil {
		in.Choices = []mergeChoice{}
	}
	sort.Slice(in.Choices, func(i, j int) bool { return in.Choices[i].Path < in.Choices[j].Path })
	args, _ := json.Marshal(in)
	digest := hash(args)
	replay := func(q rowQuery) (bool, error) {
		b, e := mergeReplay(ctx, q, unit, dev, in.Request, digest)
		if e != nil {
			return false, e
		}
		if b != nil {
			return true, reply(w, 200, json.RawMessage(b))
		}
		return false, nil
	}
	if done, e := replay(s.db); done || e != nil {
		return e
	}
	m, a, z, b, e := s.prepareMerge(ctx, unit, in.Branch)
	if e != nil {
		// A competing identical merge may already have committed and removed
		// its now-merged branch under the unit's retention policy.
		if done, replayErr := replay(s.db); done || replayErr != nil {
			return replayErr
		}
		return e
	}
	if m.plan.Main != in.Main || m.plan.Already {
		if done, e := replay(s.db); done || e != nil {
			return e
		}
		return conflict("主线已变化或分叉已经合并，请重新预览")
	}
	now := time.Now().UTC()
	m.stamp = now.UnixNano()
	m.choices = make(map[string]mergeChoice, len(in.Choices))
	for _, c := range in.Choices {
		if !validPath(c.Path) || c.Path == "" {
			return bad("invalid merge path")
		}
		if _, ok := m.choices[c.Path]; ok {
			return bad("duplicate merge path")
		}
		m.choices[c.Path] = c
	}
	root, e := m.directory("", b.Root, a.Root, z.Root, 0)
	if e != nil {
		return e
	}
	if len(m.choices) > 0 {
		return bad("选择中含有非冲突路径")
	}
	attachments := joinedAssets(a.Attachments, z.Attachments, in.Branch)
	refs, e := m.reachable(root, attachments)
	if e != nil {
		return e
	}
	for o, data := range m.created {
		if e = s.install(o, data); e != nil {
			return e
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var role, head string
	e = tx.QueryRowContext(ctx, `SELECT g.role,u.head FROM effective_grants g JOIN units u ON u.id=g.unit JOIN devices d ON d.id=g.device WHERE u.id=? AND g.device=? AND d.revoked=0`, unit, dev).Scan(&role, &head)
	if e != nil {
		return e
	}
	if role != "sync" {
		return forbidden()
	}
	if done, e := replay(tx); done || e != nil {
		return e
	}
	if head != in.Main {
		return conflict("主线已变化，请重新预览")
	}
	id := newID()
	at := stamp(now)
	record := fields{"unitId": unit, "snapshotId": id, "deviceId": dev, "parentSnapshotId": in.Main, "mergeParentSnapshotId": in.Branch, "resolvedForkSnapshotId": in.Branch, "scopeRevision": 1, "rootDigest": root, "capturedAt": at, "committedAt": at, "outcome": "published", "label": in.Label, "checkpoint": true, "attachments": attachments, "links": []any{}}
	raw, _ := json.Marshal(record)
	if _, e = tx.ExecContext(ctx, "INSERT INTO snapshots(unit,id,root,record) VALUES(?,?,?,?)", unit, id, root, raw); e != nil {
		return e
	}
	stmt, e := tx.PrepareContext(ctx, "INSERT INTO refs VALUES(?,?,?,?)")
	if e != nil {
		return e
	}
	defer stmt.Close()
	for o := range refs {
		if _, e = stmt.ExecContext(ctx, unit, id, o.Kind, o.Digest); e != nil {
			return e
		}
	}
	rec, _ := json.Marshal(fields{"unitId": unit, "snapshotId": id, "rootDigest": root, "outcome": "published", "headAtCommit": id, "committedAt": at})
	if _, e = tx.ExecContext(ctx, "UPDATE units SET head=? WHERE id=?", id, unit); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO merge_requests VALUES(?,?,?,?,?)", unit, dev, in.Request, digest, rec); e != nil {
		return e
	}
	if e = s.projectNewHistory(ctx, tx, unit, id); e != nil {
		return e
	}
	if e = s.applyPolicy(ctx, tx, unit, now); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return reply(w, 200, json.RawMessage(rec))
}
