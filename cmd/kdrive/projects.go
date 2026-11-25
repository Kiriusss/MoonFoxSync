package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"kirius.local/kiriusdrive/client/engine"
)

type includes []string

func (s *includes) String() string     { return strings.Join(*s, ", ") }
func (s *includes) Set(v string) error { *s = append(*s, v); return nil }

// CLI users can keep repeating --include. Desktop clients stream their selection
// to avoid the per-process command-line length limit on Windows.
func (s *includes) readStdin() error {
	dec := json.NewDecoder(os.Stdin)
	var paths []string
	if err := dec.Decode(&paths); err != nil || paths == nil {
		return errors.New("--includes-stdin 需要一个 JSON 路径数组")
	}
	var tail json.RawMessage
	if err := dec.Decode(&tail); err != io.EOF {
		return errors.New("路径数组末尾含有多余内容")
	}
	*s = append(*s, paths...)
	return nil
}

var requestUUID = regexp.MustCompile("^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$")
var keySyntax = regexp.MustCompile("^[a-zA-Z0-9._:-]{1,160}$")

func projectKey(k string) string {
	k = strings.TrimSpace(k)
	if keySyntax.MatchString(k) {
		return k
	}
	sum := sha256.Sum256([]byte(k))
	return "name:" + hex.EncodeToString(sum[:])
}

func projects(ctx context.Context, s *engine.Store, args []string) (any, error) {
	const usage = "project discover|ignored|git|list|import|includes|checkout|status"
	if len(args) == 0 {
		return nil, errors.New(usage)
	}
	switch args[0] {
	case "git-sync":
		return gitSync(ctx, s, args[1:])
	case "merge-plan", "merge-read", "merge-commit":
		return mergeCommands(ctx, s, args)
	case "discover", "ignored", "git":
		f := flags("project " + args[0])
		p := f.String("path", "", "")
		min := f.Int64("min-bytes", 0, "")
		fetch := f.Bool("fetch", false, "")
		if e := parse(f, args[1:]); e != nil {
			return nil, e
		}
		if *p == "" {
			return nil, errors.New("请选择目录")
		}
		switch args[0] {
		case "discover":
			return engine.DiscoverProjects(ctx, *p)
		case "ignored":
			if *min < 0 {
				return nil, errors.New("文件大小不能为负数")
			}
			files, e := engine.IgnoredFiles(ctx, *p, *min)
			return map[string]any{"items": files}, e
		default:
			ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			return s.InspectProjectGit(ctx, *p, *fetch)
		}
	case "status":
		f := flags("project status")
		refresh := f.Bool("refresh-git", false, "")
		if e := parse(f, args[1:]); e != nil {
			return nil, e
		}
		return s.ProjectStates(ctx, *refresh)
	case "list", "import", "includes", "checkout":
		if len(args) < 2 {
			return nil, errors.New("需要资料库 ID")
		}
		id := args[1]
		switch args[0] {
		case "list":
			if len(args) != 2 {
				return nil, errors.New(usage)
			}
			r, e := s.Remote(ctx)
			if e != nil {
				return nil, e
			}
			return r.Projects(ctx, id)
		case "checkout":
			f := flags("project checkout")
			to := f.String("to", "", "")
			if e := parse(f, args[2:]); e != nil {
				return nil, e
			}
			if *to == "" {
				return nil, errors.New("请指定用于恢复项目的新目录")
			}
			return s.CheckoutProject(ctx, id, *to)
		case "includes":
			f := flags("project includes")
			set := f.Bool("set", false, "")
			mode := f.String("ignore-mode", "", "all 默认包含或者 selected 默认排除 Git 忽略文件")
			var files includes
			stream := f.Bool("includes-stdin", false, "从标准输入读取 JSON 相对路径数组")
			f.Var(&files, "include", "")
			if e := parse(f, args[2:]); e != nil {
				return nil, e
			}
			if (len(files) > 0 || *stream || *mode != "") && !*set {
				return nil, errors.New("请指定 --set 以保存选择")
			}
			if *stream {
				if e := files.readStdin(); e != nil {
					return nil, e
				}
			}
			if *set {
				if *mode == "" {
					*mode = "selected"
				}
				if e := s.SetIgnoredPolicy(ctx, id, engine.IgnoredPolicy{Mode: *mode, Items: files}); e != nil {
					return nil, e
				}
			}
			return s.IgnoredPolicy(ctx, id)
		default:
			f := flags("project import")
			p := f.String("path", "", "")
			k := f.String("key", "", "")
			name := f.String("name", "", "")
			base := f.String("base", "", "")
			existing := f.String("existing", "", "")
			create := f.Bool("new", false, "")
			request := f.String("request", "", "")
			prev := f.String("rebind-from", "", "")
			mode := f.String("ignore-mode", "", "all 默认包含或者 selected 默认排除 Git 忽略文件")
			var files includes
			stream := f.Bool("includes-stdin", false, "从标准输入读取 JSON 相对路径数组")
			f.Var(&files, "include", "")
			if e := parse(f, args[2:]); e != nil {
				return nil, e
			}
			if *p == "" {
				return nil, errors.New("请选择已识别的项目目录")
			}
			if *stream {
				if e := files.readStdin(); e != nil {
					return nil, e
				}
			}
			project, e := engine.DescribeProject(ctx, *p)
			if e != nil {
				return nil, e
			}
			if (*create && *existing != "") || ((*create || *existing != "") && *k != "") || (*create != (*request != "")) {
				return nil, errors.New("请选择已有项目，或使用 --new 和独立请求 ID 新建")
			}
			if *create && !requestUUID.MatchString(*request) {
				return nil, errors.New("新建项目需要 UUID 格式的 --request，重试时使用原值")
			}
			if strings.TrimSpace(*name) == "" {
				*name = project.Name
			}
			r, e := s.Remote(ctx)
			if e != nil {
				return nil, e
			}
			var info engine.ProjectInfo
			switch {
			case *existing != "":
				info, e = r.ExistingProject(ctx, id, *existing)
			case *create:
				info, e = r.NewProject(ctx, id, strings.ToLower(*request), *name, project.SourceKey)
			default:
				if strings.TrimSpace(*k) == "" {
					*k = project.SourceKey
				}
				if strings.TrimSpace(*k) == "" {
					return nil, errors.New("请选择已有 NAS 项目，或明确新建独立项目")
				}
				info, e = r.EnsureProject(ctx, id, projectKey(*k), *name)
			}
			if e != nil {
				return nil, e
			}
			var requested *engine.IgnoredPolicy
			if *mode != "" || *stream || len(files) > 0 {
				if *mode == "" {
					*mode = "selected"
				}
				requested = &engine.IgnoredPolicy{Mode: *mode, Items: files}
			}
			b, e := s.ImportProject(ctx, *prev, info.UnitID, project.Path, *base, requested)
			if e != nil {
				return nil, fmt.Errorf("绑定共享项目 %s：%w", info.Name, e)
			}
			return map[string]any{"project": info, "binding": b}, nil
		}
	}
	return nil, errors.New(usage)
}
