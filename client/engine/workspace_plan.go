package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

type WorkPlan struct {
	Items   []WorkspaceFile `json:"items"`
	Skipped []string        `json:"skipped"`
}

func canonicalWorkspacePath(name string) (string, error) {
	if name == "" || !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return "", errors.New("请选择原工作目录中的绝对路径")
	}
	// Bind canonicalizes the OS aliases. Preserve them for Finder/CLI callers
	// while never resolving a selected placeholder into the mounted volume.
	if runtime.GOOS == "darwin" {
		for _, alias := range []string{"/var", "/tmp", "/etc"} {
			if name == alias || strings.HasPrefix(name, alias+"/") {
				actual, err := filepath.EvalSymlinks(alias)
				if err != nil {
					return "", err
				}
				if actual == "/private"+alias {
					name = actual + strings.TrimPrefix(name, alias)
				}
				break
			}
		}
	}
	parent := filepath.Dir(name)
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	if real != parent {
		return "", errors.New("不跟随工作目录中的上级符号链接")
	}
	return name, nil
}

func (s *Store) workspaceSource(ctx context.Context, name string) (Binding, string, error) {
	var selected Binding
	for _, segment := range strings.Split(filepath.ToSlash(name), "/") {
		if strings.EqualFold(segment, ".git") {
			return selected, "", errors.New("无法释放 Git 内部文件")
		}
	}
	bindings, err := s.Bindings(ctx)
	if err != nil {
		return selected, "", err
	}
	for _, b := range bindings {
		switch b.Profile {
		case "file":
			if name != b.Path {
				continue
			}
		case "project":
			if !inside(b.Path, name) {
				continue
			}
		default:
			continue
		}
		if selected.Unit != "" {
			return Binding{}, "", errors.New("多个本机绑定覆盖此路径，请先核对项目关联")
		}
		selected = b
	}
	if selected.Unit == "" {
		return selected, "", errors.New("此路径不在本机已绑定的文件夹或单文件来源中")
	}
	member, err := filepath.Rel(boundDirectory(selected.Path, selected.Profile), name)
	if err != nil {
		return selected, "", err
	}
	if member == "." && selected.Profile == "project" {
		return selected, "", nil
	}
	member = filepath.ToSlash(member)
	if !validMember(member) {
		return selected, "", errors.New("无法释放 Git 内部文件或不受支持的路径")
	}
	return selected, member, nil
}

// Decode each immutable directory once per plan. A sibling lookup is then a
// binary search of the sorted manifest rather than a fresh traversal from disk.
type workspaceIndex struct {
	load  objectfmt.Load
	dirs  map[string][]objectfmt.Entry
	files map[string]workspaceDigest
}
type workspaceDigest struct {
	sha  string
	size int64
}

func (idx *workspaceIndex) entry(root, member string) (objectfmt.Entry, error) {
	var e objectfmt.Entry
	if !validMember(member) {
		return e, errors.New("需要选择具体文件")
	}
	parts := strings.Split(member, "/")
	for i, name := range parts {
		entries, ok := idx.dirs[root]
		if !ok {
			var err error
			entries, err = objectfmt.ReadDir(root, idx.load)
			if err != nil {
				return e, err
			}
			idx.dirs[root] = entries
		}
		n := sort.Search(len(entries), func(j int) bool { return entries[j].Name >= name })
		if n == len(entries) || entries[n].Name != name {
			return e, errors.New("未备份：已上传版本不含此文件")
		}
		e = entries[n]
		if i+1 < len(parts) && e.Kind != "directory" {
			return e, errors.New("未备份：已上传版本中的上级路径不是目录")
		}
		root = e.Digest
	}
	if e.Kind != "file" {
		return e, errors.New("仅可释放已备份的普通文件")
	}
	return e, nil
}
func (idx *workspaceIndex) content(key string) (workspaceDigest, error) {
	if data, ok := idx.files[key]; ok {
		return data, nil
	}
	info, err := objectfmt.ReadFile(key, idx.load)
	if err != nil {
		return workspaceDigest{}, err
	}
	data := workspaceDigest{info.SHA256, info.Size}
	idx.files[key] = data
	return data, nil
}
func (s *Store) entryInSnapshot(root, member string) (objectfmt.Entry, error) {
	index := workspaceIndex{load: s.load, dirs: map[string][]objectfmt.Entry{}}
	return index.entry(root, member)
}

type workspacePlanner struct {
	store    *Store
	binding  Binding
	root     string
	snapshot Snapshot
	device   string
	archives map[string]WorkspaceFile
	tracked  workspaceTracked
	markers  map[string]string
	index    workspaceIndex
}

type workspaceTracked struct {
	names      map[string]bool
	identities map[[2]uint64]bool
}

func (t workspaceTracked) contains(path string, info os.FileInfo) bool {
	if t.names[path] {
		return true
	}
	id, ok := workspaceGitIdentity(info)
	return ok && t.identities[id]
}

// Query once per selected tree. On macOS the index and the actual directory
// may use different case or Unicode spellings for the same filesystem entry.
func trackedWorkspacePaths(ctx context.Context, base string, folder bool) (workspaceTracked, error) {
	tracked := workspaceTracked{names: map[string]bool{}, identities: map[[2]uint64]bool{}}
	for d := base; ; d = filepath.Dir(d) {
		_, err := os.Lstat(filepath.Join(d, ".git"))
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return tracked, err
		}
		if filepath.Dir(d) == d {
			return tracked, nil
		}
	}
	pattern := "."
	if !folder {
		// Include all indexed siblings so the spelling chosen by Finder does
		// not restrict Git's spelling by accident. The glob excludes children.
		pattern = ":(glob)*"
	}
	cmd := gitCommand(ctx, base, "ls-files", "-z", "--cached", "--", pattern)
	cmd.Env = slices.DeleteFunc(cmd.Env, func(assignment string) bool {
		name, _, _ := strings.Cut(assignment, "=")
		switch name {
		case "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CEILING_DIRECTORIES",
			"GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS":
			return true
		}
		return false
	})
	raw, err := cmd.Output()
	if err != nil {
		return tracked, fmt.Errorf("不能确认 Git 跟踪状态：%w", err)
	}
	for _, file := range strings.Split(string(raw), "\x00") {
		if file == "" {
			continue
		}
		if !validMember(file) {
			return tracked, errors.New("Git 索引中存在无效路径")
		}
		name := filepath.Join(base, filepath.FromSlash(file))
		tracked.names[name] = true
		if runtime.GOOS == "darwin" {
			info, err := os.Lstat(name)
			if os.IsNotExist(err) {
				continue // staged removal or sparse checkout
			}
			if err != nil {
				return tracked, err
			}
			if id, ok := workspaceGitIdentity(info); ok {
				tracked.identities[id] = true
			}
		}
	}
	return tracked, nil
}

func (s *Store) newWorkspacePlanner(ctx context.Context, source string, folder bool) (*workspacePlanner, error) {
	b, _, err := s.workspaceSource(ctx, source)
	if err != nil {
		return nil, err
	}
	root := boundDirectory(b.Path, b.Profile)
	real, err := filepath.EvalSymlinks(root)
	if err != nil || real != root {
		return nil, errors.New("绑定目录已移动或被符号链接替换")
	}
	p := &workspacePlanner{store: s, binding: b, root: root, markers: map[string]string{}, archives: map[string]WorkspaceFile{}, index: workspaceIndex{s.load, map[string][]objectfmt.Entry{}, map[string]workspaceDigest{}}}
	var marker string
	if err = s.db.QueryRowContext(ctx, `SELECT r.identity,a.device FROM binding_roots r JOIN account a ON a.id=1 WHERE r.unit=?`, b.Unit).Scan(&marker, &p.device); err != nil {
		return nil, err
	}
	current, err := directoryMarkerAt(root)
	if err != nil || marker != current {
		return nil, errors.New("工作目录已移动或被替换")
	}
	p.markers[root] = current
	// Follow only the binding's ancestry. Queued descendants do not invalidate
	// an unchanged file in a previous uploaded version.
	seen := map[string]bool{}
	for head := b.Base; head != nil; {
		if seen[*head] {
			return nil, errors.New("本机快照父版本出现循环")
		}
		seen[*head] = true
		snap, err := s.snap(ctx, *head)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		if snap.Unit != b.Unit {
			return nil, errors.New("本地快照归属和绑定不一致")
		}
		if snap.State == "published" || snap.State == "fork" {
			p.snapshot = snap
			break
		}
		head = snap.Parent
	}
	base := filepath.Dir(source)
	if folder {
		base = source
	}
	// Skip nested repositories just like capture.entry; never use an outer
	// repository's index to decide whether to replace an inner tracked file.
	for d := base; d != root; d = filepath.Dir(d) {
		if filepath.Dir(d) == d || !inside(root, d) {
			return nil, errors.New("所选路径超出绑定目录")
		}
		if _, e := os.Lstat(filepath.Join(d, ".git")); e == nil {
			return nil, errors.New("嵌套 Git 项目不属于这个备份来源")
		} else if !os.IsNotExist(e) {
			return nil, e
		}
	}
	p.tracked, err = trackedWorkspacePaths(ctx, base, folder)
	if err != nil {
		return nil, err
	}
	if folder {
		rows, err := s.listWorkspace(ctx, b.Unit)
		if err != nil {
			return nil, err
		}
		for _, a := range rows {
			p.archives[a.Source] = a
		}
	} else {
		a, e := s.sourceRecord(ctx, source)
		if e == nil {
			p.archives[source] = a
		} else if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
	}
	return p, nil
}

func (p *workspacePlanner) file(source string, info os.FileInfo) (WorkspaceFile, error) {
	b := p.binding
	member, err := filepath.Rel(p.root, source)
	if err != nil {
		return WorkspaceFile{}, err
	}
	member = filepath.ToSlash(member)
	if !validMember(member) {
		return WorkspaceFile{}, errors.New("仅支持普通工作区文件")
	}
	if a, exists := p.archives[source]; exists {
		if a.Unit != b.Unit || a.Generation != b.generation || a.Device != p.device || a.Member != member {
			return WorkspaceFile{}, errors.New("按需记录与当前绑定或设备不一致")
		}
		if a.State == "online" {
			target, e := os.Readlink(source)
			if e != nil || target != p.store.workspaceTarget(a) {
				return WorkspaceFile{}, errors.New("已释放文件的原位置占位已改变，请先取回")
			}
		}
		return a, nil
	}
	if !info.Mode().IsRegular() {
		return WorkspaceFile{}, errors.New("仅支持普通文件")
	}
	if p.tracked.contains(source, info) {
		return WorkspaceFile{}, errors.New("Git 已跟踪此文件；原位置释放会改变它的文件类型")
	}
	if p.snapshot.ID == "" {
		return WorkspaceFile{}, errors.New("未备份：请先保存快照并上传到 NAS")
	}
	e, err := p.index.entry(p.snapshot.Root, member)
	if err != nil {
		return WorkspaceFile{}, err
	}
	data, err := p.index.content(e.Digest)
	if err != nil {
		return WorkspaceFile{}, err
	}
	executable := info.Mode().Perm()&0111 != 0
	if info.Size() != data.size || executable != e.Executable {
		return WorkspaceFile{}, errors.New("文件已修改，请重新保存并上传")
	}
	parent := filepath.Dir(source)
	marker, ok := p.markers[parent]
	if !ok {
		real, e := filepath.EvalSymlinks(parent)
		if e != nil || real != parent {
			return WorkspaceFile{}, errors.New("上级目录已被替换")
		}
		marker, err = directoryMarkerAt(parent)
		if err != nil {
			return WorkspaceFile{}, err
		}
		p.markers[parent] = marker
	}
	return WorkspaceFile{Unit: b.Unit, Snapshot: p.snapshot.ID, Member: member, Source: source, SHA: data.sha, Size: data.size, State: "local", Device: p.device, Generation: b.generation, Entry: e, Directory: marker}, nil
}

func (s *Store) workspacePlan(ctx context.Context, source string) (WorkspaceFile, error) {
	name, err := canonicalWorkspacePath(source)
	if err != nil {
		return WorkspaceFile{}, err
	}
	p, err := s.newWorkspacePlanner(ctx, name, false)
	if err != nil {
		return WorkspaceFile{}, err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return WorkspaceFile{}, err
	}
	return p.file(name, info)
}

// The preview uses size, mode and captured modification time. The destructive
// operation subsequently checks the whole-file SHA against the pinned version.
func (p *WorkPlan) add(a WorkspaceFile, info os.FileInfo) error {
	if a.State == "local" && a.Entry.MtimeNS != math.MinInt64 && a.Entry.MtimeNS != info.ModTime().UnixNano() {
		return errors.New("文件修改时间已变化，请重新保存并上传")
	}
	p.Items = append(p.Items, a)
	return nil
}

func (s *Store) WorkspacePlan(ctx context.Context, source string) (WorkPlan, error) {
	out := WorkPlan{Items: []WorkspaceFile{}, Skipped: []string{}}
	source, err := canonicalWorkspacePath(source)
	if err != nil {
		return out, err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return out, err
	}
	if !info.IsDir() {
		p, err := s.newWorkspacePlanner(ctx, source, false)
		if err != nil {
			return out, err
		}
		a, err := p.file(source, info)
		if err != nil {
			return out, err
		}
		err = out.add(a, info)
		return out, err
	}
	p, err := s.newWorkspacePlanner(ctx, source, true)
	if err != nil {
		return out, err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return out, err
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := filepath.Join(source, filepath.FromSlash(rel))
		if walkErr != nil {
			if rel == "." {
				return walkErr
			}
			out.Skipped = append(out.Skipped, name+" · "+walkErr.Error())
			return nil
		}
		if d.IsDir() {
			if strings.EqualFold(d.Name(), ".git") {
				return fs.SkipDir
			}
			if rel != "." {
				if _, e := root.Lstat(filepath.Join(filepath.FromSlash(rel), ".git")); e == nil {
					out.Skipped = append(out.Skipped, name+" · 嵌套 Git 项目")
					return fs.SkipDir
				} else if !os.IsNotExist(e) {
					out.Skipped = append(out.Skipped, name+" · "+e.Error())
					return fs.SkipDir
				}
			}
			return nil
		}
		if strings.EqualFold(d.Name(), ".git") {
			return nil
		}
		info, e := d.Info()
		var a WorkspaceFile
		if e == nil {
			a, e = p.file(name, info)
		}
		if e == nil {
			e = out.add(a, info)
		}
		if e != nil {
			out.Skipped = append(out.Skipped, name+" · "+e.Error())
			return nil
		}
		return nil
	})
	return out, err
}
