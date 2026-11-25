package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ProjectCandidate struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	SourceKey string `json:"sourceKey"`
	Origin    string `json:"origin"`
	Warning   string `json:"warning,omitempty"`
}
type Discovery struct {
	Items    []ProjectCandidate `json:"items"`
	Warnings []string           `json:"warnings"`
}

func sourceDirectory(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("请选择项目目录")
	}
	dir, e := filepath.Abs(dir)
	if e != nil {
		return "", e
	}
	info, e := os.Lstat(dir)
	if e != nil {
		return "", e
	}
	if !info.IsDir() {
		return "", errors.New("项目来源必须是实际目录")
	}
	return dir, nil
}
func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "maintenance.auto=false", "-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_NO_LAZY_FETCH=1")
	// Older Git builds can lazy-fetch while checking a partial clone. Local
	// metadata inspection must not silently open another transport.
	if len(args) > 0 && args[0] != "ls-remote" && args[0] != "fetch" && args[0] != "push" {
		cmd.Env = append(cmd.Env, "GIT_ALLOW_PROTOCOL=")
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}
func gitValue(ctx context.Context, dir string, args ...string) (string, error) {
	b, e := gitCommand(ctx, dir, args...).Output()
	return strings.TrimRight(string(b), "\r\n"), e
}
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// The display form is distinct from the identity material for local remotes.
// No URL passwords, user info, query parameters or machine-local paths leave this function.
func originIdentity(dir, raw string) (string, string) {
	var canonical, display string
	local := func(name string) (string, string) {
		if name == "" {
			return "", ""
		}
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		name = filepath.Clean(name)
		if resolved, e := filepath.EvalSymlinks(name); e == nil {
			name = resolved
		}
		return "local:" + name, "本地 Git 仓库"
	}
	if raw == "" || (!strings.Contains(raw, "://") && strings.Contains(raw, "::")) || strings.ContainsAny(raw, "\r\n\x00") {
		return "", ""
	}
	if strings.Contains(raw, "://") {
		u, e := url.Parse(raw)
		if e != nil {
			return "", ""
		}
		switch strings.ToLower(u.Scheme) {
		case "file":
			if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
				return "", ""
			}
			name := filepath.FromSlash(u.Path)
			if filepath.Separator == '\\' && len(name) > 2 && name[0] == '\\' && name[2] == ':' {
				name = name[1:]
			}
			canonical, display = local(name)
		case "http", "https", "ssh", "git":
			host := strings.ToLower(u.Hostname())
			if host == "" {
				return "", ""
			}
			port := u.Port()
			defaults := map[string]string{"http": "80", "https": "443", "ssh": "22", "git": "9418"}
			if port != "" && port != defaults[strings.ToLower(u.Scheme)] {
				host = net.JoinHostPort(host, port)
			} else if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			display = networkOrigin(host, u.EscapedPath())
			canonical = display
		default:
			return "", ""
		}
	} else if at := strings.Index(raw, ":"); at > 0 && !strings.ContainsAny(raw[:at], "/\\") && filepath.VolumeName(raw) == "" && !(at == 1 && len(raw) > 2 && (raw[2] == '/' || raw[2] == '\\')) {
		host := raw[:at]
		if i := strings.LastIndexByte(host, '@'); i >= 0 {
			host = host[i+1:]
		}
		repo := raw[at+1:]
		if i := strings.IndexAny(repo, "?#"); i >= 0 {
			repo = repo[:i]
		}
		display = networkOrigin(strings.ToLower(host), repo)
		canonical = display
	} else {
		canonical, display = local(raw)
	}
	if canonical == "" {
		return "", ""
	}
	return fmt.Sprintf("git:%x", sha256.Sum256([]byte(canonical))), display
}
func networkOrigin(host, repository string) string {
	if host == "" {
		return ""
	}
	// Normalize URL and SCP-style spellings to a slash-separated path.
	if unescaped, e := url.PathUnescape(repository); e == nil {
		repository = unescaped
	}
	repository = strings.TrimSuffix(strings.Trim(path.Clean("/"+repository), "/"), ".git")
	if repository == "" || strings.ContainsAny(repository, "\r\n\x00") {
		return ""
	}
	if host == "github.com" {
		repository = strings.ToLower(repository)
	}
	return host + "/" + repository
}
func projectKind(dir string) (string, error) {
	st, e := os.Lstat(filepath.Join(dir, ".git"))
	if e == nil {
		if !st.IsDir() && !st.Mode().IsRegular() {
			return "", errors.New("不跟随 .git 符号链接")
		}
		return "git", nil
	}
	if !os.IsNotExist(e) {
		return "", e
	}
	if h, err := os.Lstat(filepath.Join(dir, "HEAD")); err == nil && h.Mode().IsRegular() {
		objects, objErr := os.Lstat(filepath.Join(dir, "objects"))
		refs, refErr := os.Lstat(filepath.Join(dir, "refs"))
		if objErr == nil && refErr == nil && objects.IsDir() && refs.IsDir() {
			return "bare", nil
		}
	}
	for _, name := range []string{"go.mod", "go.work", "Cargo.toml", "package.json", "pyproject.toml", "setup.py", "CMakeLists.txt", "pom.xml", "settings.gradle", "settings.gradle.kts", "Package.swift", "build.zig", "composer.json", "Gemfile", "pubspec.yaml", "Makefile"} {
		st, e = os.Lstat(filepath.Join(dir, name))
		if e == nil && st.Mode().IsRegular() {
			return "project", nil
		}
		if e != nil && !os.IsNotExist(e) {
			return "", e
		}
	}
	return "directory", nil
}
func DescribeProject(ctx context.Context, dir string) (ProjectCandidate, error) {
	var p ProjectCandidate
	dir, e := sourceDirectory(dir)
	if e != nil {
		return p, e
	}
	kind, e := projectKind(dir)
	if e != nil {
		return p, e
	}
	if kind == "bare" {
		return p, errors.New("裸 Git 仓库没有可同步的工作树")
	}
	p = ProjectCandidate{Path: dir, Name: filepath.Base(dir), Kind: kind}
	if kind == "git" {
		raw, err := gitValue(ctx, dir, "remote", "get-url", "origin")
		if err == nil {
			p.SourceKey, p.Origin = originIdentity(dir, raw)
		}
		if p.SourceKey == "" {
			p.Warning = "此工作区没有可识别的 origin，请填写共享项目标识。"
		}
	}
	return p, ctx.Err()
}
func ignoreScanDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "build", "dist", "target", "DerivedData", "__pycache__", "venv", "Pods", "bin", "obj":
		return true
	}
	return false
}

// A marked directory owns its descendants (including monorepo packages).
// Unmarked immediate children that contain no projects remain selectable.
func DiscoverProjects(ctx context.Context, parent string) (Discovery, error) {
	result := Discovery{Items: []ProjectCandidate{}, Warnings: []string{}}
	parent, e := sourceDirectory(parent)
	if e != nil {
		return result, e
	}
	budget := 4096
	var scan func(string, int) (bool, error)
	scan = func(dir string, depth int) (bool, error) {
		if e := ctx.Err(); e != nil {
			return false, e
		}
		if budget <= 0 {
			return false, errors.New("超过 4096 个目录，请选择更小的父目录")
		}
		budget--
		kind, e := projectKind(dir)
		if e != nil {
			return false, e
		}
		if kind == "bare" {
			result.Warnings = append(result.Warnings, "已跳过裸 Git 仓库 "+dir)
			return true, nil
		}
		if kind != "directory" {
			p, e := DescribeProject(ctx, dir)
			if e != nil {
				return false, e
			}
			result.Items = append(result.Items, p)
			return true, nil
		}
		if depth == 3 {
			return false, nil
		}
		entries, e := os.ReadDir(dir)
		if e != nil {
			return false, e
		}
		found := false
		for _, entry := range entries {
			if !entry.IsDir() || ignoreScanDir(entry.Name()) {
				continue
			}
			child := filepath.Join(dir, entry.Name())
			yes, e := scan(child, depth+1)
			if e != nil {
				if budget <= 0 || ctx.Err() != nil {
					return false, e
				}
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s: %v", child, e))
				// Do not select a fallback ancestor while some descendants were unreadable.
				found = true
				continue
			}
			if !yes && depth == 0 {
				result.Items = append(result.Items, ProjectCandidate{Path: child, Name: entry.Name(), Kind: "directory"})
				yes = true
			}
			found = found || yes
		}
		return found, nil
	}
	_, e = scan(parent, 0)
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].Path < result.Items[j].Path })
	return result, e
}

type GitState struct {
	State        string `json:"state"`
	Branch       string `json:"branch"`
	Upstream     string `json:"upstream"`
	Head         string `json:"head"`
	UpstreamHead string `json:"upstreamHead"`
	Ahead        int    `json:"ahead"`
	Behind       int    `json:"behind"`
	Dirty        bool   `json:"dirty"`
	Fetched      bool   `json:"fetched"`
}

func InspectGit(ctx context.Context, dir string, fetch bool) (GitState, error) {
	return inspectGit(ctx, dir, fetch, fetch)
}

// InspectGitLight reads one upstream branch reference. A newly advertised
// commit stays on the remote until the user explicitly requests history.
func InspectGitLight(ctx context.Context, dir string) (GitState, error) {
	return inspectGit(ctx, dir, true, false)
}

func inspectGit(ctx context.Context, dir string, fetch, downloadObjects bool) (GitState, error) {
	var s GitState
	dir, e := sourceDirectory(dir)
	if e != nil {
		return s, e
	}
	kind, e := projectKind(dir)
	if e != nil {
		return s, e
	}
	if kind != "git" {
		s.State = "not_git"
		return s, nil
	}
	dirty, e := gitValue(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignore-submodules=dirty")
	if e != nil {
		return s, errors.New("无法读取 Git 工作区状态，请确认已安装 Git 并有目录访问权限")
	}
	s.Dirty = dirty != ""
	s.Branch, _ = gitValue(ctx, dir, "symbolic-ref", "-q", "--short", "HEAD")
	s.Head, e = gitValue(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
	if e != nil {
		s.State = "unborn"
		return s, nil
	}
	if s.Branch == "" {
		s.State = "detached"
		return s, nil
	}
	// Read the configured remote and merge ref even if it has never been fetched.
	remote, _ := gitValue(ctx, dir, "config", "--get", "branch."+s.Branch+".remote")
	merge, _ := gitValue(ctx, dir, "config", "--get", "branch."+s.Branch+".merge")
	if remote == "" || !strings.HasPrefix(merge, "refs/") {
		s.State = "no_upstream"
		return s, nil
	}
	s.Upstream = remote + "/" + strings.TrimPrefix(merge, "refs/heads/")
	if fetch && remote != "." {
		netCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
		defer cancel()
		// Read the advertised remote tip, then fetch only its objects. No local branch,
		// remote-tracking ref, FETCH_HEAD, index or worktree is updated by this operation.
		raw, err := gitValue(netCtx, dir, "ls-remote", "--exit-code", "--", remote, merge)
		if err != nil {
			if ctx.Err() != nil {
				return s, ctx.Err()
			}
			if exitCode(err) == 2 {
				s.State = "upstream_missing"
			} else {
				s.State = "offline"
			}
			return s, nil
		}
		fields := strings.Fields(raw)
		if len(fields) != 2 || fields[1] != merge || !validGitHash(fields[0]) {
			return s, errors.New("Git 上游返回了意外的引用")
		}
		s.UpstreamHead = fields[0]
		if _, e = gitValue(netCtx, dir, "cat-file", "-e", s.UpstreamHead+"^{commit}"); e != nil {
			if !downloadObjects {
				s.State = "remote_changed"
				s.Fetched = true
				return s, nil
			}
			_, e = gitValue(netCtx, dir, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--refmap=", "--", remote, s.UpstreamHead)
			if e != nil {
				if ctx.Err() != nil {
					return s, ctx.Err()
				}
				s.State = "offline"
				return s, nil
			}
		}
		s.Fetched = true
	} else {
		ref := "@{upstream}^{commit}"
		if remote == "." {
			ref = merge + "^{commit}"
		}
		s.UpstreamHead, e = gitValue(ctx, dir, "rev-parse", "--verify", ref)
		if e != nil {
			s.State = "upstream_missing"
			return s, nil
		}
		s.Fetched = fetch
	}
	counts, e := gitValue(ctx, dir, "rev-list", "--left-right", "--count", s.Head+"..."+s.UpstreamHead)
	fields := strings.Fields(counts)
	if e != nil || len(fields) != 2 {
		return s, errors.New("Git 无法比较两个提交的历史")
	}
	s.Ahead, e = strconv.Atoi(fields[0])
	if e != nil {
		return s, e
	}
	s.Behind, e = strconv.Atoi(fields[1])
	if e != nil {
		return s, e
	}
	switch {
	case s.Ahead > 0 && s.Behind > 0:
		s.State = "diverged"
	case s.Ahead > 0:
		s.State = "ahead"
	case s.Behind > 0:
		s.State = "behind"
	default:
		s.State = "equal"
	}
	return s, nil
}
func validGitHash(v string) bool {
	if len(v) != 40 && len(v) != 64 {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
