package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"kirius.local/kiriusdrive/client/engine"
)

const help = `心狐同步 · kdrive
  pair --server <https://nas:7443> [--ca ca.pem] [--code-file path]
  project discover|ignored|git --path <dir> [--fetch / --min-bytes N]
  project list <共享空间ID>
  project git-sync --path <dir> [--apply --expected-head <SHA> --expected-upstream <SHA> --plan-token <令牌>]
  library delete-plan|forget-plan <资料库ID> | library reset-plan
  library delete <资料库ID> --name <原名> --token <令牌> | library forget <资料库ID> --token <令牌> | library reset --token <令牌>
  project import <共享空间ID> --path <dir> (--existing <目标项目ID> | --new --request <UUID> | --key <共享标识>) [--base <已核对版本>] [--include <Git忽略的相对路径>]
  project status [--refresh-git]
  project checkout <资料库ID> --to <新目录>
  project includes <资料库ID> [--set --include <相对路径>]
  project merge-plan <项目ID> --branch <分叉版本>
  project merge-read <项目ID> --snapshot <版本> --path <成员>
  project merge-commit <项目ID> --main <主线版本> --branch <分叉版本> --request <UUID> [--name 说明]（从标准输入读取 JSON 选择数组）
  preset codex status|disable|run
  preset codex enable [--scope <资料库ID>] [--policy network|home|manual] [--interval 30]
  schedule get <来源ID> | schedule set <来源ID> --policy network|home|manual --interval <分钟>
  timeline policy <资料库ID>
  timeline preview-policy|policy <资料库ID> --mode all|latest|days --value N [--head <当前版本>]
  timeline preview-delete <资料库ID> --snapshot <时间点> --head <当前版本>
  timeline delete <资料库ID> --snapshot <时间点> --head <当前版本> --plan-token <预览令牌>
  timeline restore <资料库ID> --snapshot <时间点> --head <当前版本> --request <UUID> [--name 说明]
  status | units
  unit bind <unitId> --path <dir> [--profile project|file] [--preview] [--base <verified-snapshot>]
  bindings
  cache list | cache fetch|evict <unitId> <snapshotId> --path <relative-file>
  snapshot <unitId> [--name label]
  settings transfers [--uploads N] [--downloads M]
  queue | transfers
  sync-pending --now [--home-url <https://家庭地址:端口>] [--home-interface <网卡>]
  sync <unitId> --snapshot <snapshotId> --now
  history <unitId>
  ls <unitId> [--snapshot <snapshotId>] [--path relative]
  get <unitId> --path <relative-file> --to <new-file>
  restore <unitId> <snapshotId> --to <new-directory> [--path relative]

--json 为默认输出格式。KDRIVE_HOME 可指定独立设备状态目录。
配对码从标准输入或 --code-file 读取。设备凭据默认存入系统凭据库。`

func flags(cmd string) *flag.FlagSet {
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("多余参数: %q", f.Arg(0))
	}
	return nil
}
func run(ctx context.Context, args []string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New(help)
	}
	if args[0] == "transfers" {
		if len(args) != 1 {
			return nil, errors.New(help)
		}
		items, err := engine.ActiveTransfers("")
		return map[string]any{"items": items}, err
	}
	open := engine.Open
	if len(args) >= 2 && args[0] == "library" && (args[1] == "forget-plan" || args[1] == "reset-plan") {
		open = engine.OpenRemovalPreview
	}
	s, e := open("")
	if e != nil {
		return nil, e
	}
	defer s.Close()
	switch args[0] {
	case "settings":
		if len(args) < 2 || args[1] != "transfers" {
			return nil, errors.New(help)
		}
		if len(args) == 2 {
			return s.TransferSettings(ctx)
		}
		f := flags("settings transfers")
		up := f.Int("uploads", 0, "")
		down := f.Int("downloads", 0, "")
		if e = parse(f, args[2:]); e != nil {
			return nil, e
		}
		var u, d *int
		f.Visit(func(o *flag.Flag) {
			if o.Name == "uploads" {
				u = up
			} else {
				d = down
			}
		})
		return s.SetTransferSettings(ctx, u, d)
	case "library":
		return libraryCommands(ctx, s, args[1:])
	case "timeline":
		return timeline(ctx, s, args[1:])
	case "preset":
		return preset(ctx, s, args[1:])
	case "schedule":
		return schedule(ctx, s, args[1:])
	case "project":
		return projects(ctx, s, args[1:])
	case "status":
		if len(args) != 1 {
			return nil, errors.New(help)
		}
		return s.LocalStatus(ctx)
	case "pair":
		f := flags("pair")
		server := f.String("server", "", "")
		caFile := f.String("ca", "", "")
		codeFile := f.String("code-file", "", "")
		name := f.String("name", "心狐同步", "")
		if e = parse(f, args[1:]); e != nil {
			return nil, e
		}
		var ca, code []byte
		if *caFile != "" {
			ca, e = os.ReadFile(*caFile)
			if e != nil {
				return nil, e
			}
		}
		if *codeFile != "" && *codeFile != "-" {
			code, e = os.ReadFile(*codeFile)
		} else {
			fmt.Fprint(os.Stderr, "请输入一次性配对码：")
			line, err := bufio.NewReader(io.LimitReader(os.Stdin, 1024)).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			code = []byte(line)
		}
		if e != nil {
			return nil, e
		}
		return s.Pair(ctx, *server, *name, strings.TrimSpace(string(code)), ca)
	case "cache":
		if len(args) == 2 && args[1] == "list" {
			items, e := s.Cached(ctx)
			return map[string]any{"items": items}, e
		}
		if len(args) < 4 || (args[1] != "fetch" && args[1] != "evict") {
			return nil, errors.New(help)
		}
		f := flags("cache")
		member := f.String("path", "", "")
		if e = parse(f, args[4:]); e != nil {
			return nil, e
		}
		return s.Cache(ctx, args[1], args[2], args[3], *member)
	case "unit":
		if len(args) < 3 || args[1] != "bind" {
			return nil, errors.New(help)
		}
		f := flags("bind")
		path := f.String("path", "", "")
		profile := f.String("profile", "project", "")
		preview := f.Bool("preview", false, "")
		base := f.String("base", "", "")
		if e = parse(f, args[3:]); e != nil {
			return nil, e
		}
		if *path == "" {
			return nil, errors.New("缺少 --path")
		}
		return s.Bind(ctx, args[2], *path, *profile, *preview, *base)
	case "snapshot":
		if len(args) < 2 {
			return nil, errors.New(help)
		}
		f := flags("snapshot")
		label := f.String("name", "", "")
		if e = parse(f, args[2:]); e != nil {
			return nil, e
		}
		return s.Snapshot(ctx, args[1], *label)
	case "queue":
		if len(args) != 1 {
			return nil, errors.New(help)
		}
		items, e := s.Queue(ctx)
		return map[string]any{"items": items}, e
	case "bindings":
		if len(args) != 1 {
			return nil, errors.New(help)
		}
		items, e := s.Bindings(ctx)
		return map[string]any{"items": items}, e
	case "sync-pending":
		f := flags("sync-pending")
		now := f.Bool("now", false, "")
		home := f.String("home-url", "", "")
		iface := f.String("home-interface", "", "")
		if e = parse(f, args[1:]); e != nil {
			return nil, e
		}
		if !*now {
			return nil, errors.New("请用 --now 确认立即同步待传输版本")
		}
		return s.SyncPending(ctx, engine.AutoPolicy{HomeURL: *home, Interface: *iface})
	case "sync":
		if len(args) < 2 {
			return nil, errors.New(help)
		}
		f := flags("sync")
		id := f.String("snapshot", "", "")
		now := f.Bool("now", false, "")
		if e = parse(f, args[2:]); e != nil {
			return nil, e
		}
		if !*now || *id == "" {
			return nil, errors.New("请用 --snapshot <id> --now 指定本次手动放行版本")
		}
		return s.Sync(ctx, args[1], *id)
	case "units", "history", "ls", "get", "restore":
		r, e := s.Remote(ctx)
		if e != nil {
			return nil, e
		}
		switch args[0] {
		case "units":
			if len(args) != 1 {
				return nil, errors.New(help)
			}
			return r.Units(ctx)
		case "history":
			if len(args) != 2 {
				return nil, errors.New(help)
			}
			return r.History(ctx, args[1])
		case "ls":
			if len(args) < 2 {
				return nil, errors.New(help)
			}
			f := flags("ls")
			path := f.String("path", "", "")
			id := f.String("snapshot", "", "")
			if e = parse(f, args[2:]); e != nil {
				return nil, e
			}
			return r.Entries(ctx, args[1], *id, *path)
		case "get":
			if len(args) < 2 {
				return nil, errors.New(help)
			}
			f := flags("get")
			to := f.String("to", "", "")
			member := f.String("path", "", "")
			if e = parse(f, args[2:]); e != nil {
				return nil, e
			}
			return r.Get(ctx, args[1], *member, *to)
		case "restore":
			if len(args) < 3 {
				return nil, errors.New(help)
			}
			f := flags("restore")
			to := f.String("to", "", "")
			member := f.String("path", "", "")
			if e = parse(f, args[3:]); e != nil {
				return nil, e
			}
			return r.Restore(ctx, args[1], args[2], *member, *to)
		}
	}
	return nil, errors.New(help)
}
func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Println(help)
		return
	}
	filtered := []string{}
	for _, arg := range args {
		if arg != "--json" {
			filtered = append(filtered, arg)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	out, e := run(ctx, filtered)
	if e != nil {
		fmt.Fprintln(os.Stderr, "心狐同步:", e)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if e = enc.Encode(out); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
