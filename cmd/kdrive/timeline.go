package main

import (
	"context"
	"errors"
	"kirius.local/kiriusdrive/client/engine"
)

func timeline(ctx context.Context, s *engine.Store, args []string) (any, error) {
	if len(args) < 2 {
		return nil, errors.New("timeline policy|preview-policy|delete|restore <资料库ID>")
	}
	op, unit := args[0], args[1]
	r, err := s.Remote(ctx)
	if err != nil {
		return nil, err
	}
	if op == "policy" && len(args) == 2 {
		return r.HistoryPolicy(ctx, unit)
	}
	f := flags("timeline " + op)
	head := f.String("head", "", "")
	switch op {
	case "policy", "preview-policy":
		mode := f.String("mode", "", "")
		val := f.Int("value", 0, "")
		if e := parse(f, args[2:]); e != nil {
			return nil, e
		}
		p := engine.HistoryPolicy{Mode: *mode, Value: *val}
		if op == "preview-policy" {
			return r.PreviewPolicy(ctx, unit, p)
		}
		return r.SetHistoryPolicy(ctx, unit, p, *head)
	case "delete", "restore", "preview-delete":
		snap := f.String("snapshot", "", "")
		if op == "preview-delete" {
			if e := parse(f, args[2:]); e != nil {
				return nil, e
			}
			return r.PreviewDelete(ctx, unit, *snap, *head)
		}
		if op == "delete" {
			token := f.String("plan-token", "", "")
			if e := parse(f, args[2:]); e != nil {
				return nil, e
			}
			return r.DeletePoint(ctx, unit, *snap, *head, *token)
		}
		req := f.String("request", "", "")
		label := f.String("name", "", "")
		if e := parse(f, args[2:]); e != nil {
			return nil, e
		}
		return r.RestorePoint(ctx, unit, *snap, *head, requestULID(*req), *label)
	}
	return nil, errors.New("未知时间线操作")
}
