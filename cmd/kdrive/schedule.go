package main

import (
	"context"
	"errors"
	"kirius.local/kiriusdrive/client/engine"
)

func preset(ctx context.Context, s *engine.Store, args []string) (any, error) {
	if len(args) < 2 || args[0] != "codex" {
		return nil, errors.New("请选择 codex 预设")
	}
	if args[1] == "enable" {
		f := flags("preset codex enable")
		scope := f.String("scope", "", "")
		policy := f.String("policy", "network", "")
		n := f.Int("interval", 30, "")
		if e := parse(f, args[2:]); e != nil {
			return nil, e
		}
		return s.EnableCodex(ctx, *scope, *policy, *n)
	}
	if len(args) != 2 {
		return nil, errors.New("预设操作含有多余参数")
	}
	switch args[1] {
	case "status":
		return s.CodexStatus(ctx)
	case "disable":
		return s.DisableCodex(ctx)
	case "run":
		return s.RunCodex(ctx)
	}
	return nil, errors.New("请选择 status、enable、disable 或 run")
}
func schedule(ctx context.Context, s *engine.Store, args []string) (any, error) {
	if len(args) == 2 && args[0] == "get" {
		return s.Schedule(ctx, args[1])
	}
	if len(args) < 2 || args[0] != "set" {
		return nil, errors.New("schedule get|set <unitId>")
	}
	f := flags("schedule set")
	p := f.String("policy", "", "")
	n := f.Int("interval", 0, "")
	if e := parse(f, args[2:]); e != nil {
		return nil, e
	}
	return s.SetSchedule(ctx, args[1], *p, *n)
}
