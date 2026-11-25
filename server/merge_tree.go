package server

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

type mergeChoice struct {
	Path   string  `json:"path"`
	Choice string  `json:"choice"`
	Text   *string `json:"text,omitempty"`
}
type mergeConflict struct {
	Path        string `json:"path"`
	MainKind    string `json:"mainKind"`
	BranchKind  string `json:"branchKind"`
	BaseKind    string `json:"baseKind"`
	BothPath    string `json:"bothPath"`
	TextAllowed bool   `json:"textAllowed"`
}
type mergePlan struct {
	BaseHidden bool            `json:"baseHidden,omitempty"`
	Ambiguous  bool            `json:"baseAmbiguous,omitempty"`
	Unit       string          `json:"unitId"`
	Main       string          `json:"mainHead"`
	Branch     string          `json:"branchSnapshotId"`
	Base       string          `json:"baseSnapshotId"`
	Already    bool            `json:"alreadyMerged"`
	Conflicts  []mergeConflict `json:"conflicts"`
	Auto       []string        `json:"autoPaths"`
}
type merger struct {
	ctx        context.Context
	s          *Store
	plan       mergePlan
	choices    map[string]mergeChoice // nil only for preview
	created    map[objectfmt.Ref][]byte
	normalized map[string]string
	stamp      int64
}

func (m *merger) put(o objectfmt.Ref, b []byte) error {
	if m.choices != nil {
		m.created[o] = b
	}
	return m.ctx.Err()
}
func (m *merger) load(o objectfmt.Ref) ([]byte, error) {
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	if b, ok := m.created[o]; ok {
		return b, nil
	}
	return m.s.read(o)
}
func (m *merger) dirEntries(id string) (map[string]objectfmt.Entry, error) {
	entries := map[string]objectfmt.Entry{}
	if id != "" {
		v, e := objectfmt.ReadDir(id, m.load)
		if e != nil {
			return nil, e
		}
		for _, item := range v {
			entries[item.Name] = item
		}
	}
	return entries, nil
}
func (m *merger) equal(a, b objectfmt.Entry) (bool, error) {
	a.MtimeNS, b.MtimeNS = 0, 0
	if a == b {
		return true, nil
	}
	if a.Kind != "directory" || b.Kind != "directory" {
		return false, nil
	}
	normalize := func(id string) (string, error) {
		if known, ok := m.normalized[id]; ok {
			return known, nil
		}
		got, e := m.s.contentFingerprintMemo(m.ctx, id, m.normalized)
		return got, e
	}
	left, e := normalize(a.Digest)
	if e != nil {
		return false, e
	}
	right, e := normalize(b.Digest)
	return left == right, e
}
func kind(e objectfmt.Entry) string {
	if e.Kind == "" {
		return "missing"
	}
	return e.Kind
}

const mergeTextLimit = 128 << 10

func validMergeText(b []byte) bool {
	return len(b) <= mergeTextLimit && utf8.Valid(b) && bytes.IndexByte(b, 0) < 0
}
func (m *merger) allowText(entries ...objectfmt.Entry) (bool, error) {
	for _, v := range entries {
		if v.Kind == "" {
			continue
		}
		if v.Kind != "file" {
			return false, nil
		}
		f, e := objectfmt.ReadFile(v.Digest, m.load)
		if e != nil {
			return false, e
		}
		if f.Size > mergeTextLimit {
			return false, nil
		}
		var body []byte
		for _, chunk := range f.Chunks {
			b, e := m.load(chunk)
			if e != nil {
				return false, e
			}
			body = append(body, b...)
		}
		if !validMergeText(body) {
			return false, nil
		}
	}
	return true, nil
}
func bothName(name, branch string, used map[string]bool) string {
	ext := path.Ext(name)
	if ext == name || len(ext) > 45 {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		suffix := " (fork-" + branch[len(branch)-8:] + ")"
		if i > 1 {
			suffix = fmt.Sprintf(" (fork-%s-%d)", branch[len(branch)-8:], i)
		}
		cut := len(stem)
		for cut+len(suffix)+len(ext) > 255 {
			_, n := utf8.DecodeLastRuneInString(stem[:cut])
			cut -= n
		}
		candidate := stem[:cut] + suffix + ext
		key := strings.ToLower(candidate)
		if !used[key] {
			used[key] = true
			return candidate
		}
	}
}

func (m *merger) directory(folder, baseID, mainID, branchID string, depth int) (string, error) {
	if depth > 512 {
		return "", bad("merge directory depth exceeds limit")
	}
	base, e := m.dirEntries(baseID)
	if e != nil {
		return "", e
	}
	main, e := m.dirEntries(mainID)
	if e != nil {
		return "", e
	}
	branch, e := m.dirEntries(branchID)
	if e != nil {
		return "", e
	}
	used := map[string]bool{}
	names := map[string]bool{}
	for _, source := range []map[string]objectfmt.Entry{base, main, branch} {
		for n := range source {
			used[strings.ToLower(n)] = true
			names[n] = true
		}
	}
	order := make([]string, 0, len(names))
	for n := range names {
		order = append(order, n)
	}
	sort.Strings(order)
	out := make([]objectfmt.Entry, 0, len(names))
	for _, n := range order {
		if e := m.ctx.Err(); e != nil {
			return "", e
		}
		p := path.Join(folder, n)
		b, a, z := base[n], main[n], branch[n]
		winner := a
		sameSides, e := m.equal(a, z)
		if e != nil {
			return "", e
		}
		sameMain, e := m.equal(b, a)
		if e != nil {
			return "", e
		}
		sameBranch, e := m.equal(b, z)
		if e != nil {
			return "", e
		}
		switch {
		case sameSides:
			if !sameMain {
				m.plan.Auto = append(m.plan.Auto, p)
			}
		case sameMain && !m.plan.Ambiguous:
			winner = z
			m.plan.Auto = append(m.plan.Auto, p)
		case sameBranch && !m.plan.Ambiguous:
			m.plan.Auto = append(m.plan.Auto, p)
		case a.Kind == "directory" && z.Kind == "directory" && (b.Kind == "directory" || b.Kind == ""):
			winner.Digest, e = m.directory(p, b.Digest, a.Digest, z.Digest, depth+1)
			if e != nil {
				return "", e
			}
		default:
			c := mergeConflict{Path: p, MainKind: kind(a), BranchKind: kind(z), BaseKind: kind(b)}
			if a.Kind != "" && z.Kind != "" {
				c.BothPath = path.Join(folder, bothName(n, m.plan.Branch, used))
			}
			c.TextAllowed, e = m.allowText(b, a, z)
			if e != nil {
				return "", e
			}
			m.plan.Conflicts = append(m.plan.Conflicts, c)
			if m.choices == nil {
				break
			}
			choice, ok := m.choices[p]
			if !ok {
				return "", bad("缺少冲突处理选择：" + p)
			}
			delete(m.choices, p)
			if choice.Choice != "text" && choice.Text != nil {
				return "", bad("正文只适用于 text 选择")
			}
			switch choice.Choice {
			case "main":
				winner = a
			case "branch":
				winner = z
			case "both":
				if c.BothPath == "" {
					return "", bad("删除冲突不能保留两份")
				}
				copy := z
				copy.Name = path.Base(c.BothPath)
				out = append(out, copy)
			case "text":
				if !c.TextAllowed || choice.Text == nil || !validMergeText([]byte(*choice.Text)) {
					return "", bad("无效的手动合并文本")
				}
				if winner.Kind == "" {
					winner = z
				}
				winner.MtimeNS = m.stamp
				winner.Digest, e = objectfmt.BuildFile(strings.NewReader(*choice.Text), m.put)
				if e != nil {
					return "", e
				}
			default:
				return "", bad("未知的冲突处理选择")
			}
		}
		if winner.Kind != "" {
			out = append(out, winner)
		}
	}
	return objectfmt.BuildDir(out, m.put)
}

// Existing snapshot contents were verified before their original publication.
// Collect exactly the merged tree's metadata and chunk references without
// touching large immutable payloads. Newly edited text objects were hashed above.
func (m *merger) reachable(root string, attachments []asset) (map[objectfmt.Ref]bool, error) {
	seen := map[objectfmt.Ref]bool{}
	load := func(o objectfmt.Ref) ([]byte, error) { seen[o] = true; return m.load(o) }
	walkedFile := map[string]bool{}
	walkedTree := map[string]bool{}
	var file func(string) error
	file = func(id string) error {
		if walkedFile[id] {
			return nil
		}
		walkedFile[id] = true
		f, e := objectfmt.ReadFile(id, load)
		if e != nil {
			return e
		}
		for _, c := range f.Chunks {
			seen[c] = true
		}
		return nil
	}
	var tree func(string, int) error
	tree = func(id string, depth int) error {
		if depth > 512 {
			return bad("merge directory depth exceeds limit")
		}
		if walkedTree[id] {
			return nil
		}
		walkedTree[id] = true
		entries, e := objectfmt.ReadDir(id, load)
		if e != nil {
			return e
		}
		for _, n := range entries {
			if n.Kind == "file" {
				e = file(n.Digest)
			} else if n.Kind == "directory" {
				e = tree(n.Digest, depth+1)
			}
			if e != nil {
				return e
			}
		}
		return nil
	}
	if e := tree(root, 0); e != nil {
		return nil, e
	}
	for _, a := range attachments {
		if e := file(a.FileDigest); e != nil {
			return nil, e
		}
	}
	return seen, nil
}
