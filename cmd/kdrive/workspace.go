package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"kirius.local/kiriusdrive/client/engine"
)

func workspaceCommand(ctx context.Context, s *engine.Store, args []string) (any, error) {
	if len(args) == 1 && args[0] == "release-batch" {
		var plans []engine.WorkspaceFile
		if err := json.NewDecoder(os.Stdin).Decode(&plans); err != nil {
			return nil, err
		}
		failed := []string{}
		done := 0
		for _, p := range plans {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if _, err := s.WorkspaceRelease(ctx, p.Source, p.Snapshot, p.SHA); err != nil {
				failed = append(failed, p.Source+" · "+err.Error())
			} else {
				done++
			}
		}
		return map[string]any{"completed": done, "errors": failed}, nil
	}
	if len(args) == 1 && args[0] == "list" {
		v, e := s.WorkspaceFiles(ctx)
		return map[string]any{"items": v}, e
	}
	if len(args) < 2 {
		return nil, errors.New("workspace list | plan --source … | release --source … --snapshot … --sha … | hydrate --source …")
	}
	f := flags("workspace")
	source := f.String("source", "", "")
	snapshot := f.String("snapshot", "", "")
	sha := f.String("sha", "", "")
	if e := parse(f, args[1:]); e != nil {
		return nil, e
	}
	if *source == "" {
		return nil, errors.New("请选择原位置的文件")
	}
	switch args[0] {
	case "plan":
		return s.WorkspacePlan(ctx, *source)
	case "release":
		return s.WorkspaceRelease(ctx, *source, *snapshot, *sha)
	case "hydrate":
		return s.WorkspaceHydrate(ctx, *source)
	}
	return nil, errors.New("未知的原位置操作")
}
