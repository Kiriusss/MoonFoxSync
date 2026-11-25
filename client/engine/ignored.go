package engine

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

type IgnoredFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func nulField(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errors.New("Git 返回了不完整的路径")
	}
	return 0, nil, nil
}
func validMember(rel string) bool {
	if !fs.ValidPath(rel) || rel == "." || strings.ContainsRune(rel, 0) || !filepath.IsLocal(filepath.FromSlash(rel)) || filepath.ToSlash(filepath.FromSlash(rel)) != rel {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}
func ignoredRegular(root *os.Root, rel string) (os.FileInfo, error) {
	if !validMember(rel) {
		return nil, errors.New("无效的项目内相对路径")
	}
	name := filepath.FromSlash(rel)
	// Do not cross symlinked ancestors or a nested repository boundary.
	for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
		i, e := root.Lstat(dir)
		if e != nil {
			return nil, e
		}
		if !i.IsDir() {
			return nil, errors.New("所选文件的上级路径不是实际目录")
		}
		if _, e = root.Lstat(filepath.Join(dir, ".git")); e == nil {
			return nil, errors.New("所选路径属于嵌套 Git 仓库")
		} else if !os.IsNotExist(e) {
			return nil, e
		}
	}
	return root.Lstat(name)
}

func IgnoredFiles(ctx context.Context, dir string, minBytes int64) ([]IgnoredFile, error) {
	out := []IgnoredFile{}
	if minBytes < 0 {
		return out, errors.New("文件大小阈值必须非负")
	}
	dir, e := sourceDirectory(dir)
	if e != nil {
		return out, e
	}
	k, e := projectKind(dir)
	if e != nil || k != "git" {
		return out, e
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		return out, e
	}
	defer root.Close()
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCommand(commandCtx, dir, "ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--", ".")
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		return out, e
	}
	if e = cmd.Start(); e != nil {
		return out, e
	}
	scanner := bufio.NewScanner(pipe)
	scanner.Split(nulField)
	for scanner.Scan() {
		member := scanner.Text()
		if !validMember(member) {
			continue
		}
		info, err := ignoredRegular(root, member)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			e = err
			break
		}
		if (info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) && info.Size() >= minBytes {
			out = append(out, IgnoredFile{member, info.Size()})
		}
	}
	if e == nil {
		e = scanner.Err()
	}
	if e != nil {
		cancel()
	}
	if err := cmd.Wait(); e == nil {
		e = err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, e
}

type policyReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// In all mode Items are exceptions. In selected mode they are inclusions.
type IgnoredPolicy struct {
	Mode  string   `json:"mode"`
	Items []string `json:"items"`
}

func defaultIgnored() IgnoredPolicy {
	return IgnoredPolicy{Mode: "all", Items: []string{}}
}

func (p IgnoredPolicy) equal(other IgnoredPolicy) bool {
	return p.Mode == other.Mode && slices.Equal(p.Items, other.Items)
}

func readIgnored(ctx context.Context, q policyReader, unit string) (IgnoredPolicy, error) {
	policy := defaultIgnored()
	var raw string
	err := q.QueryRowContext(ctx, "SELECT paths FROM ignored_selection WHERE unit=?", unit).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, nil
	}
	if err != nil {
		return policy, err
	}
	// 0.3.0 stored only an explicit allowlist. Its empty list meant select none.
	if strings.HasPrefix(strings.TrimSpace(raw), "[") {
		policy.Mode = "selected"
		err = json.Unmarshal([]byte(raw), &policy.Items)
	} else {
		err = json.Unmarshal([]byte(raw), &policy)
	}
	if err == nil && (policy.Mode != "selected" && policy.Mode != "all" || policy.Items == nil) {
		err = errors.New("忽略文件策略无效")
	}
	return policy, err
}

func (s *Store) IgnoredPolicy(ctx context.Context, unit string) (IgnoredPolicy, error) {
	if _, err := s.binding(ctx, unit); err != nil {
		return IgnoredPolicy{}, err
	}
	return readIgnored(ctx, s.db, unit)
}

func (s *Store) IgnoredSelection(ctx context.Context, unit string) ([]string, error) {
	p, err := s.IgnoredPolicy(ctx, unit)
	return p.Items, err
}

func (s *Store) SetIgnoredSelection(ctx context.Context, unit string, paths []string) error {
	return s.SetIgnoredPolicy(ctx, unit, IgnoredPolicy{Mode: "selected", Items: paths})
}

// Only paths currently ignored can override Git's regular tracked/untracked
// set. A saved exception automatically stops applying when the file is tracked.
func ignoredMatches(ctx context.Context, dir string, paths []string) (map[string]bool, error) {
	matched := make(map[string]bool)
	if len(paths) == 0 {
		return matched, nil
	}
	var input bytes.Buffer
	for _, member := range paths {
		if !validMember(member) {
			return nil, errors.New("无效的项目内相对路径")
		}
		input.WriteString("./" + member)
		input.WriteByte(0)
	}
	cmd := gitCommand(ctx, dir, "check-ignore", "--stdin", "-z")
	cmd.Stdin = &input
	raw, err := cmd.Output()
	if err != nil && exitCode(err) != 1 {
		return nil, fmt.Errorf("无法验证 Git 忽略规则: %w", err)
	}
	for _, member := range strings.Split(string(raw), "\x00") {
		if member != "" {
			matched[strings.TrimPrefix(member, "./")] = true
		}
	}
	return matched, nil
}

func validateIgnoredPolicy(ctx context.Context, dir string, policy, previous IgnoredPolicy) (IgnoredPolicy, error) {
	if policy.Mode != "all" && policy.Mode != "selected" {
		return policy, errors.New("请选择 all 或 selected 忽略文件策略")
	}
	policy.Items = append([]string{}, policy.Items...)
	sort.Strings(policy.Items)
	policy.Items = slices.Compact(policy.Items)
	if len(policy.Items) == 0 {
		return policy, nil
	}
	kind, err := projectKind(dir)
	if err != nil {
		return policy, err
	}
	if kind != "git" {
		return policy, errors.New("此目录没有 Git 工作区")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return policy, err
	}
	defer root.Close()
	var check []string
	for _, member := range policy.Items {
		if !validMember(member) {
			return policy, errors.New("无效的项目内相对路径")
		}
		if _, wasSaved := slices.BinarySearch(previous.Items, member); wasSaved && previous.Mode == policy.Mode {
			continue // Keep unchanged choices even if temporarily absent.
		}
		info, err := ignoredRegular(root, member)
		if err != nil {
			return policy, fmt.Errorf("检查 %s: %w", member, err)
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return policy, fmt.Errorf("请选择文件: %s", member)
		}
		check = append(check, member)
	}
	matched, err := ignoredMatches(ctx, dir, check)
	if err != nil {
		return policy, err
	}
	if len(matched) != len(check) {
		return policy, errors.New("所选文件有一项已不在 Git 忽略范围内，请刷新列表")
	}
	return policy, nil
}

func (s *Store) SetIgnoredPolicy(ctx context.Context, unit string, policy IgnoredPolicy) error {
	b, e := s.binding(ctx, unit)
	if e != nil {
		return e
	}
	if b.Profile != "project" {
		return errors.New("忽略文件选择仅适用于项目工作区")
	}
	previous, e := readIgnored(ctx, s.db, unit)
	if e != nil {
		return e
	}
	policy, e = validateIgnoredPolicy(ctx, b.Path, policy, previous)
	if e != nil {
		return e
	}
	if previous.equal(policy) {
		return nil
	}
	raw, e := json.Marshal(policy)
	if e != nil {
		return e
	}
	var existingProof bool
	if b.Base != nil {
		if e = s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM project_proofs WHERE unit=? AND base=?)", unit, *b.Base).Scan(&existingProof); e != nil {
			return e
		}
	}
	var observed string
	if existingProof {
		if observed, e = baselineFingerprint(ctx, b, policy); e != nil {
			return e
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	result, e := tx.ExecContext(ctx, `INSERT INTO ignored_selection(unit,paths) SELECT unit,? FROM bindings WHERE unit=? AND path=? AND base IS ? AND generation=? ON CONFLICT(unit) DO UPDATE SET paths=excluded.paths`, string(raw), unit, b.Path, b.Base, b.generation)
	if e != nil {
		return e
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return errors.New("同步基线已变化，请刷新后重新选择")
	}
	if existingProof {
		if _, e = tx.ExecContext(ctx, "UPDATE project_proofs SET observed=? WHERE unit=? AND base=?", observed, unit, *b.Base); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func writeIgnored(ctx context.Context, tx *sql.Tx, unit string, policy IgnoredPolicy) error {
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO ignored_selection(unit,paths) VALUES(?,?) ON CONFLICT(unit) DO UPDATE SET paths=excluded.paths", unit, string(raw))
	return err
}

// ImportProject commits a new association and its chosen policy together.
// Otherwise an automatic scan could back up files explicitly unchecked in the importer.
func (s *Store) ImportProject(ctx context.Context, previous, target, path, base string, requested *IgnoredPolicy) (Binding, error) {
	if requested != nil {
		from := previous
		if from == "" {
			from = target
		}
		old, err := readIgnored(ctx, s.db, from)
		if err != nil {
			return Binding{}, err
		}
		policy, err := validateIgnoredPolicy(ctx, path, *requested, old)
		if err != nil {
			return Binding{}, err
		}
		requested = &policy
	}
	var b Binding
	var err error
	if previous != "" {
		b, err = s.reassociateWithPolicy(ctx, previous, target, path, base, requested)
	} else {
		b, err = s.bindSourceWithPolicy(ctx, target, path, "project", false, base, requested)
	}
	if err != nil || requested == nil {
		return b, err
	}
	// An already-bound project may be revisited in the importer to change its
	// selection. Fresh or successfully retried bindings already match.
	if err = s.SetIgnoredPolicy(ctx, target, *requested); err != nil {
		return b, err
	}
	return s.binding(ctx, target)
}
