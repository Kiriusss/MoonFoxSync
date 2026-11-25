package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type GitSyncPlan struct {
	GitState
	PlanToken     string `json:"planToken"`
	remote, merge string
}

// Match the binding generation we observed before starting network work.
func (s *Store) SyncProjectGit(ctx context.Context, dir string, apply bool, head, tip, token string) (GitSyncPlan, error) {
	var p GitSyncPlan
	dir, err := sourceDirectory(dir)
	if err != nil {
		return p, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return p, err
	}
	var unit, generation string
	err = s.db.QueryRowContext(ctx, "SELECT unit,generation FROM bindings WHERE path=? AND profile='project'", dir).Scan(&unit, &generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	if apply {
		lock, lockErr := lockFile(filepath.Join(s.Home, fmt.Sprintf("git-sync-%x.lock", sha256.Sum256([]byte(dir)))), true)
		if lockErr != nil {
			return p, fmt.Errorf("无法锁定该工作树进行同步（可能已有操作在执行）：%w", lockErr)
		}
		defer lock.Close()
		p, err = ApplyGitSync(ctx, dir, head, tip, token)
	} else {
		p, err = PreviewGitSync(ctx, dir)
	}
	if err == nil && unit != "" {
		_, err = s.saveGitState(ctx, unit, dir, generation, p.GitState)
	}
	return p, err
}

func PreviewGitSync(ctx context.Context, dir string) (GitSyncPlan, error) {
	var p GitSyncPlan
	dir, err := sourceDirectory(dir)
	if err != nil {
		return p, err
	}
	p.GitState, err = InspectGit(ctx, dir, true)
	if err != nil || (p.State != "ahead" && p.State != "behind") {
		return p, err
	}
	p.remote, err = gitValue(ctx, dir, "config", "--get", "branch."+p.Branch+".remote")
	if err != nil || p.remote == "." || strings.HasPrefix(p.remote, "-") {
		return p, errors.New("请为当前分支配置一个远端上游分支")
	}
	p.merge, err = gitValue(ctx, dir, "config", "--get", "branch."+p.Branch+".merge")
	if err != nil || !strings.HasPrefix(p.merge, "refs/heads/") || p.Upstream != p.remote+"/"+strings.TrimPrefix(p.merge, "refs/heads/") {
		return p, errors.New("上游分支已变化，请重新检查")
	}
	fetchURL, err := gitValue(ctx, dir, "remote", "get-url", "--", p.remote)
	if err != nil {
		return p, errors.New("无法读取 Git 远端地址")
	}
	if p.State == "ahead" {
		pushURL, err := gitValue(ctx, dir, "remote", "get-url", "--push", "--all", "--", p.remote)
		if err != nil || pushURL != fetchURL {
			return p, errors.New("此远端配置了独立或多个推送地址，请在 Git 中确认推送目标")
		}
		if mirror, _ := gitValue(ctx, dir, "config", "--bool", "--get", "remote."+p.remote+".mirror"); mirror == "true" {
			return p, errors.New("镜像远端会更新多个引用，请选择普通的分支上游")
		}
	}
	p.PlanToken = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join([]string{dir, p.State, p.Branch, p.Head, p.remote, p.merge, fetchURL, p.UpstreamHead}, "\x00"))))
	return p, nil
}

func ApplyGitSync(ctx context.Context, dir, head, tip, token string) (GitSyncPlan, error) {
	p, err := PreviewGitSync(ctx, dir)
	if err != nil {
		return p, err
	}
	if token == "" || token != p.PlanToken || p.Head != head || p.UpstreamHead != tip {
		return p, errors.New("分支、远端或提交已变化，请重新预览")
	}
	switch p.State {
	case "behind":
		if p.Dirty {
			return p, errors.New("请先提交改动，或用 git stash 保存工作区，再快进拉取")
		}
		// Avoid updating a worktree while a sequencer is waiting for user input.
		for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "sequencer"} {
			path, err := gitValue(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", name)
			if err != nil {
				return p, errors.New("无法检查当前工作树的操作状态")
			}
			if _, err := os.Stat(filepath.Clean(path)); err == nil || !os.IsNotExist(err) {
				return p, errors.New("工作树有未结束的 Git 操作，请先在 Git 中处理")
			}
		}
		// Check the checked-out worktree again immediately before changing it.
		// git merge also locks and compares HEAD internally when advancing the ref.
		status, err := gitValue(ctx, dir, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=normal", "--ignore-submodules=dirty")
		if err != nil {
			return p, err
		}
		var branchNow, headNow string
		for _, line := range strings.Split(status, "\x00") {
			if value, ok := strings.CutPrefix(line, "# branch.head "); ok {
				branchNow = value
			}
			if value, ok := strings.CutPrefix(line, "# branch.oid "); ok {
				headNow = value
			}
			if line != "" && !strings.HasPrefix(line, "# ") {
				return p, errors.New("工作区在预览后发生变化，请重新检查")
			}
		}
		if branchNow != p.Branch || headNow != head {
			return p, errors.New("当前分支在预览后发生变化，请重新检查")
		}
		_, err = gitValue(ctx, dir, "merge", "--ff-only", "--no-autostash", "--no-overwrite-ignore", tip)
		if err != nil {
			return p, errors.New("Git 拒绝快进。请检查工作树和可能被覆盖的忽略文件，然后重新预览")
		}
	case "ahead":
		// A plain push performs the remote server's non-fast-forward check.
		// Use the reviewed object ID, even if a new local commit appears.
		_, err = gitValue(ctx, dir, "push", "--porcelain", "--no-follow-tags", "--recurse-submodules=no", "--", p.remote, head+":"+p.merge)
		if err != nil {
			return p, errors.New("Git 推送失败，请检查远端权限、分支保护和上游的新提交，再重新预览")
		}
	default:
		return p, errors.New("当前分支没有可安全快进的同步操作")
	}
	// Update ordinary remote-tracking refs after a reviewed operation.
	if _, err = gitValue(ctx, dir, "fetch", "--no-tags", "--no-recurse-submodules", "--", p.remote, p.merge); err != nil {
		return p, errors.New("分支同步成功，但刷新远端跟踪引用失败，请重新检查上游状态")
	}
	return PreviewGitSync(ctx, dir)
}
