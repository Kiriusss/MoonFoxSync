package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ProjectState struct {
	generation   string
	ProtectHead  bool      `json:"protectHead,omitempty"`
	Unit         string    `json:"unitId"`
	Name         string    `json:"name"`
	Path         string    `json:"path,omitempty"`
	Kind         string    `json:"kind"`
	LastActivity string    `json:"lastActivity"`
	NAS          string    `json:"nasState"`
	Head         *string   `json:"head"`
	Base         *string   `json:"base"`
	Pending      int       `json:"pending"`
	Git          *GitState `json:"git,omitempty"`
	GitCheckedAt string    `json:"gitCheckedAt,omitempty"`
	GitError     string    `json:"gitError,omitempty"`
}

type ProjectReport struct {
	Items   []*ProjectState `json:"items"`
	Warning string          `json:"warning,omitempty"`
}

// ULID timestamps date a captured project version without downloading its entire history.
func captureTime(id string) time.Time {
	if !idPattern.MatchString(id) {
		return time.Time{}
	}
	const digits = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var ms int64
	for _, c := range id[:10] {
		ms = ms*32 + int64(strings.IndexRune(digits, c))
	}
	return time.UnixMilli(ms).UTC()
}

func (s *Store) ProjectStates(ctx context.Context, refreshGit bool) (ProjectReport, error) {
	result := ProjectReport{Items: []*ProjectState{}}
	bindings, err := s.Bindings(ctx)
	if err != nil {
		return result, err
	}
	snaps, err := s.Queue(ctx)
	if err != nil {
		return result, err
	}
	projects := map[string]*ProjectState{}
	for _, b := range bindings {
		if b.Profile != "project" {
			continue
		}
		projects[b.Unit] = &ProjectState{generation: b.generation, Unit: b.Unit, Name: filepath.Base(b.Path), Path: b.Path, Base: b.Base, ProtectHead: b.ProtectHead, Kind: "project", NAS: "unknown"}
	}
	byID := map[string]Snapshot{}
	firstQueued := map[string]Snapshot{}
	for _, snap := range snaps {
		if p := projects[snap.Unit]; p != nil {
			if snap.State == "queued" {
				p.Pending++
				if _, exists := firstQueued[p.Unit]; !exists {
					firstQueued[p.Unit] = snap
				}
			}
			p.LastActivity = snap.Captured
			byID[snap.ID] = snap
		}
	}
	local, err := s.LocalStatus(ctx)
	if err != nil {
		return result, err
	}
	// Keep local projects and pending work visible on an offline or unpaired device.
	if local.Paired {
		err = func() error {
			remote, e := s.Remote(ctx)
			if e != nil {
				return e
			}
			probe, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			page, e := remote.Units(probe)
			if e != nil {
				return e
			}
			for _, raw := range page.Items {
				var u struct {
					ID    string  `json:"unitId"`
					Name  string  `json:"name"`
					Kind  string  `json:"kind"`
					Scope string  `json:"scopeId"`
					Key   string  `json:"projectKey"`
					Head  *string `json:"head"`
				}
				if e = json.Unmarshal(raw, &u); e != nil {
					return e
				}
				if strings.HasPrefix(u.Key, "codex-history:") {
					continue
				}
				p := projects[u.ID]
				if p == nil {
					if u.Scope == "" || u.Kind != "managed" {
						continue
					}
					p = &ProjectState{Unit: u.ID, Kind: "project"}
					projects[u.ID] = p
				}
				p.Name = u.Name
				p.Head = u.Head
				var job Snapshot
				if p.Base != nil {
					job = byID[*p.Base]
				}
				switch {
				case p.Path == "":
					p.NAS = "server_only"
				case job.State == "fork":
					p.NAS = "fork"
				case p.ProtectHead:
					p.NAS = "protected"
				case p.Pending > 0:
					p.NAS = "pending"
					if !sameSnapshot(firstQueued[p.Unit].Parent, p.Head) {
						p.NAS = "diverged"
					}
				case p.Head == nil:
					p.NAS = "empty"
				case !sameSnapshot(p.Base, p.Head):
					p.NAS = "remote_ahead"
				case job.ID == "":
					p.NAS = "baseline"
				default:
					p.NAS = "current"
				}
				if p.Head != nil {
					t := captureTime(*p.Head)
					lt, _ := time.Parse(time.RFC3339Nano, p.LastActivity)
					if !t.IsZero() && t.After(lt) {
						p.LastActivity = t.Format(time.RFC3339Nano)
					}
				}
			}
			return nil
		}()
		if err != nil {
			result.Warning = err.Error()
		}
	}
	for _, p := range projects {
		if p.LastActivity == "" && p.Path != "" {
			if info, e := os.Stat(p.Path); e == nil {
				p.LastActivity = info.ModTime().UTC().Format(time.RFC3339Nano)
			}
		}
		result.Items = append(result.Items, p)
	}
	sort.Slice(result.Items, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, result.Items[i].LastActivity)
		b, _ := time.Parse(time.RFC3339Nano, result.Items[j].LastActivity)
		if a.Equal(b) {
			return result.Items[i].Unit < result.Items[j].Unit
		}
		return a.After(b)
	})
	ch := make(chan *ProjectState)
	var workers sync.WaitGroup
	for i := 0; i < 3; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for p := range ch {
				s.gitForReport(ctx, p, refreshGit)
			}
		}()
	}
	for _, p := range result.Items {
		if p.Path != "" {
			ch <- p
		}
	}
	close(ch)
	workers.Wait()
	return result, ctx.Err()
}

func sameSnapshot(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s *Store) gitForReport(ctx context.Context, p *ProjectState, fetch bool) {
	if _, err := os.Lstat(filepath.Join(p.Path, ".git")); err != nil {
		if !os.IsNotExist(err) {
			p.GitError = err.Error()
		}
		return
	}
	p.Kind = "git"
	var raw []byte
	var checked int64
	err := s.db.QueryRowContext(ctx, "SELECT result,checked FROM git_status_cache WHERE unit=?", p.Unit).Scan(&raw, &checked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		p.GitError = err.Error()
		return
	}
	cacheOK := false
	if err == nil {
		var prior GitState
		if json.Unmarshal(raw, &prior) == nil {
			p.Git = &prior
			p.GitCheckedAt = time.Unix(checked, 0).UTC().Format(time.RFC3339)
			cacheOK = true
		}
	}
	if cacheOK && (!fetch || time.Since(time.Unix(checked, 0)) < 15*time.Minute) {
		return
	}
	limit, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var git GitState
	var e error
	if fetch {
		git, e = InspectGitLight(limit, p.Path)
	} else {
		git, e = InspectGit(limit, p.Path, false)
	}
	if e != nil {
		p.GitError = fmt.Sprintf("Git 检查失败：%v", e)
		return
	}
	p.Git = &git
	if !fetch {
		return
	}
	stamp, e := s.saveGitState(ctx, p.Unit, p.Path, p.generation, git)
	if e != nil {
		p.GitError = e.Error()
	} else {
		p.GitCheckedAt = stamp
	}
}

func (s *Store) saveGitState(ctx context.Context, unit, path, generation string, g GitState) (string, error) {
	raw, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `INSERT INTO git_status_cache(unit,checked,result) SELECT unit,?,? FROM bindings WHERE unit=? AND path=? AND generation=? ON CONFLICT(unit) DO UPDATE SET checked=excluded.checked,result=excluded.result`, now.Unix(), raw, unit, path, generation)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if n == 0 || err != nil {
		return "", err
	}
	return now.Format(time.RFC3339), err
}

// InspectProjectGit shares manual checks with the status seen by both desktop surfaces.
func (s *Store) InspectProjectGit(ctx context.Context, dir string, fetch bool) (GitState, error) {
	if !fetch {
		return InspectGit(ctx, dir, false)
	}
	var g GitState
	full, err := filepath.Abs(dir)
	if err != nil {
		return g, err
	}
	full, err = filepath.EvalSymlinks(full)
	if err != nil {
		return g, err
	}
	var unit, generation string
	err = s.db.QueryRowContext(ctx, "SELECT unit,generation FROM bindings WHERE path=? AND profile='project'", full).Scan(&unit, &generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return g, err
	}
	g, err = InspectGit(ctx, full, true)
	if err != nil || unit == "" {
		return g, err
	}
	_, err = s.saveGitState(ctx, unit, full, generation, g)
	return g, err
}
