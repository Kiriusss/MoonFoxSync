package main

import (
	"context"
	"errors"
	"time"

	"kirius.local/kiriusdrive/client/engine"
)

func gitSync(ctx context.Context, s *engine.Store, args []string) (any, error) {
	f := flags("project git-sync")
	dir := f.String("path", "", "")
	apply := f.Bool("apply", false, "")
	head := f.String("expected-head", "", "")
	tip := f.String("expected-upstream", "", "")
	token := f.String("plan-token", "", "")
	if err := parse(f, args); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if *apply {
		if *head == "" || *tip == "" || *token == "" {
			return nil, errors.New("请先核对当前分支和上游，再确认同步")
		}
	}
	return s.SyncProjectGit(ctx, *dir, *apply, *head, *tip, *token)
}
