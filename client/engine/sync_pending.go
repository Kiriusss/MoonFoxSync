package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"
)

type PendingSyncResult struct {
	Total    int      `json:"total"`
	Uploaded int      `json:"uploaded"`
	Active   int      `json:"active"`
	Skipped  int      `json:"skipped"`
	Failed   int      `json:"failed"`
	Errors   []string `json:"errors"`
}

// Probe the pinned, on-link endpoint before choosing it. Explicit manual
// release also permits the ordinary paired endpoint, without a traffic budget.
func (s *Store) manualRemote(ctx context.Context, p AutoPolicy) (*Remote, bool, error) {
	if p.HomeURL != "" && p.Interface != "" {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		home, err := s.homeRemote(probe, p)
		if err == nil {
			err = home.call(probe, "GET", "/units", url.Values{"limit": {"1"}}, nil, nil)
			if err == nil {
				return home, true, nil
			}
		}
	}
	paired, err := s.Remote(ctx)
	return paired, false, err
}

func (s *Store) SyncPending(ctx context.Context, policy AutoPolicy) (PendingSyncResult, error) {
	result := PendingSyncResult{Errors: []string{}}
	activity, err := s.localLock(false)
	if err != nil {
		return result, err
	}
	defer activity.Close()
	// A single SQLite statement fixes this run's membership. Captures can
	// continue in parallel and will be picked up by a subsequent run.
	items, err := s.readSnapshots(ctx, "SELECT "+snapCols+" FROM snapshots WHERE state='queued' ORDER BY captured,id")
	if err != nil || len(items) == 0 {
		return result, err
	}
	result.Total = len(items)
	bindings, err := s.Bindings(ctx)
	if err != nil {
		return result, err
	}
	names := make(map[string]string, len(bindings))
	for _, b := range bindings {
		names[b.Unit] = filepath.Base(b.Path)
	}
	remote, home, err := s.manualRemote(ctx, policy)
	if err != nil {
		return result, err
	}
	send := func(v Snapshot, request string) (*Receipt, error) {
		receipt, err := s.sendSnapshot(ctx, remote, v, request)
		var network *url.Error
		if home && ctx.Err() == nil && errors.As(err, &network) {
			// Route changes and lost responses are recoverable with the same
			// request ID against the paired service.
			paired, lookupErr := s.Remote(ctx)
			if lookupErr != nil {
				return nil, lookupErr
			}
			remote, home = paired, false
			return s.sendSnapshot(ctx, remote, v, request)
		}
		return receipt, err
	}

	queued := make(map[string]bool, len(items))
	children := make(map[string][]Snapshot)
	for _, v := range items {
		queued[v.ID] = true
	}
	var ready []Snapshot
	for _, v := range items {
		if v.Parent != nil && queued[*v.Parent] {
			children[*v.Parent] = append(children[*v.Parent], v)
		} else {
			ready = append(ready, v)
		}
	}
	blocked := make(map[string]bool)
	for i := 0; i < len(ready); i++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		v := ready[i]
		ready = append(ready, children[v.ID]...)
		if v.Parent != nil && blocked[*v.Parent] {
			blocked[v.ID] = true
			result.Skipped++
			continue
		}
		_, reused, err := s.withUpload(ctx, v.Unit, v.ID, send)
		switch {
		case errors.Is(err, errUploadActive):
			blocked[v.ID] = true
			result.Active++
		case err != nil:
			blocked[v.ID] = true
			result.Failed++
			name := names[v.Unit]
			if name == "" {
				name = v.Unit
			}
			result.Errors = append(result.Errors, fmt.Sprintf("%s（版本 %s）：%v", name, v.ID, err))
		case reused:
			result.Skipped++
		default:
			result.Uploaded++
		}
	}
	if cyclic := result.Total - len(ready); cyclic > 0 {
		result.Failed += cyclic
		result.Errors = append(result.Errors, fmt.Sprintf("%d 个版本的父版本依赖存在循环", cyclic))
	}
	return result, nil
}
