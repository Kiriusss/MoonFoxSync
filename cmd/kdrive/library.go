package main

import (
	"context"
	"errors"
	"kirius.local/kiriusdrive/client/engine"
)

func libraryCommands(ctx context.Context, s *engine.Store, args []string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("请选择资料库操作")
	}
	if args[0] != "delete" && args[0] != "delete-plan" {
		return localLibrary(ctx, s, args)
	}
	if len(args) < 2 {
		return nil, errors.New("需要资料库 ID")
	}
	r, err := s.Remote(ctx)
	if err != nil {
		return nil, err
	}
	if args[0] == "delete-plan" {
		if len(args) != 2 {
			return nil, errors.New("请指定一个资料库 ID")
		}
		return r.LibraryDeletePlan(ctx, args[1])
	}
	f := flags("library delete")
	name := f.String("name", "", "")
	token := f.String("token", "", "")
	if err := parse(f, args[2:]); err != nil {
		return nil, err
	}
	return r.DeleteLibrary(ctx, args[1], *name, *token)
}
