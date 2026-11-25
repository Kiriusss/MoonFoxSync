package main

import (
	"context"
	"errors"

	"kirius.local/kiriusdrive/client/engine"
)

func localLibrary(ctx context.Context, s *engine.Store, args []string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("需要本机资料库操作")
	}
	op := args[0]
	rest := args[1:]
	unit := ""
	switch op {
	case "forget-plan", "forget":
		if len(rest) == 0 || rest[0] == "" {
			return nil, errors.New("需要本机资料库 ID")
		}
		unit, rest = rest[0], rest[1:]
	case "reset-plan", "reset":
	default:
		return nil, errors.New("未知的本机资料库操作")
	}
	if op == "reset-plan" || op == "forget-plan" {
		if len(rest) != 0 {
			return nil, errors.New("预览不支持额外参数")
		}
		return s.LocalRemovalPlan(ctx, unit)
	}
	f := flags("library " + op)
	token := f.String("token", "", "本机删除预览中的确认码")
	if err := parse(f, rest); err != nil {
		return nil, err
	}
	return s.RemoveLocal(ctx, unit, *token)
}
