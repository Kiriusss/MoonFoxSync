package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type CodexPreset struct {
	Enabled    bool   `json:"enabled"`
	Policy     string `json:"policy"`
	Minutes    int    `json:"intervalMinutes"`
	LastBackup string `json:"lastBackup"`
	LastError  string `json:"lastError"`
	Root       string `json:"root"`
	Unit       string `json:"unitId"`
	Scope      string `json:"scopeId"`
}

func codexHome() (string, error) {
	if root := os.Getenv("CODEX_HOME"); root != "" {
		return filepath.Abs(root)
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(home, ".codex"), nil
}
func (s *Store) CodexStatus(ctx context.Context) (CodexPreset, error) {
	v := CodexPreset{Policy: "network", Minutes: 30}
	var e error
	v.Root, e = codexHome()
	if e != nil {
		return v, e
	}
	e = s.db.QueryRowContext(ctx, `SELECT c.enabled,c.unit,c.scope,b.path,s.policy,s.minutes,c.last_error,COALESCE(
 (SELECT json_extract(receipt,'$.committedAt') FROM snapshots WHERE unit=c.unit AND receipt IS NOT NULL ORDER BY captured DESC,id DESC LIMIT 1),'')
 FROM codex_preset c JOIN bindings b ON b.unit=c.unit JOIN schedules s ON s.unit=c.unit WHERE c.id=1`).Scan(&v.Enabled, &v.Unit, &v.Scope, &v.Root, &v.Policy, &v.Minutes, &v.LastError, &v.LastBackup)
	if errors.Is(e, sql.ErrNoRows) {
		e = nil
	}
	return v, e
}
func (s *Store) presetLock() (*os.File, error) {
	l, e := cacheLock(filepath.Join(s.Home, "codex-preset.lock"))
	if e != nil {
		return nil, fmt.Errorf("Codex 历史任务正在执行或无法锁定：%w", e)
	}
	return l, nil
}
func (s *Store) EnableCodex(ctx context.Context, scope, policy string, minutes int) (CodexPreset, error) {
	var v CodexPreset
	if e := validateSchedule(policy, minutes); e != nil {
		return v, e
	}
	l, e := s.presetLock()
	if e != nil {
		return v, e
	}
	defer l.Close()
	v, e = s.CodexStatus(ctx)
	if e != nil {
		return v, e
	}
	if v.Unit != "" && scope != "" && v.Scope != scope {
		return v, errors.New("此历史来源已固定到先前选择的资料库")
	}
	if v.Unit == "" {
		st, e := os.Lstat(v.Root)
		if e != nil {
			return v, fmt.Errorf("无法打开 Codex 历史目录 %s：%w", v.Root, e)
		}
		if !st.IsDir() || codexReparse(st) {
			return v, errors.New("请将 CODEX_HOME 指向实际的历史目录，不能选择符号链接或联接点")
		}
		r, e := s.Remote(ctx)
		if e != nil {
			return v, e
		}
		var units []unitInfo
		if e = r.unitList(ctx, &units); e != nil {
			return v, e
		}
		var scopes []string
		for _, u := range units {
			if u.Kind == "managed" && u.Role == "sync" && u.ScopeID == "" && (scope == "" || scope == u.ID) {
				scopes = append(scopes, u.ID)
			}
		}
		if len(scopes) != 1 {
			return v, errors.New("请选择一个有同步权限的目标资料库")
		}
		scope = scopes[0]
		var device string
		if e = s.db.QueryRowContext(ctx, "SELECT device FROM account WHERE id=1").Scan(&device); e != nil {
			return v, e
		}
		name, _ := os.Hostname()
		if name == "" {
			name = device
		}
		info, e := r.EnsureProject(ctx, scope, "codex-history:"+device, "Codex 对话历史 · "+name)
		if e != nil {
			return v, e
		}
		// A retry may encounter our binding after interruption. Never adopt an
		// unrelated, already-populated remote timeline as an implicit baseline.
		if info.Head != nil {
			existing, err := s.binding(ctx, info.UnitID)
			if err != nil || existing.Profile != "codex" {
				return v, errors.New("目标历史已存在，请保留原设备配置或使用新的配对设备")
			}
		}
		b, e := s.bindSource(ctx, info.UnitID, v.Root, "codex", false, baseID(info.Head))
		if e != nil {
			return v, e
		}
		v.Unit, v.Scope = b.Unit, scope
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return v, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, writeSchedule, v.Unit, policy, minutes); e != nil {
		return v, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO codex_preset(id,unit,scope,enabled) VALUES(1,?,?,1) ON CONFLICT(id) DO UPDATE SET enabled=1,last_error=''`, v.Unit, v.Scope); e != nil {
		return v, e
	}
	if e = tx.Commit(); e != nil {
		return v, e
	}
	return s.CodexStatus(ctx)
}
func (s *Store) DisableCodex(ctx context.Context) (CodexPreset, error) {
	l, e := s.presetLock()
	if e != nil {
		return CodexPreset{}, e
	}
	defer l.Close()
	if _, e = s.db.ExecContext(ctx, "UPDATE codex_preset SET enabled=0 WHERE id=1"); e != nil {
		return CodexPreset{}, e
	}
	return s.CodexStatus(ctx)
}
func (s *Store) recordPresetError(ctx context.Context, e error) {
	msg := ""
	if e != nil {
		msg = e.Error()
	}
	s.db.ExecContext(ctx, "UPDATE codex_preset SET last_error=? WHERE id=1", msg)
}
func (s *Store) RunCodex(ctx context.Context) (v CodexPreset, err error) {
	l, e := s.presetLock()
	if e != nil {
		return v, e
	}
	defer l.Close()
	v, e = s.CodexStatus(ctx)
	if e != nil {
		return v, e
	}
	if v.Unit == "" {
		return v, errors.New("请先启用 Codex 对话历史并选择保存位置")
	}
	defer func() {
		if err != nil {
			s.recordPresetError(ctx, err)
		}
	}()
	if _, e = s.Snapshot(ctx, v.Unit, ""); e != nil {
		return v, e
	}
	jobs, e := s.Queue(ctx)
	if e != nil {
		return v, e
	}
	for _, j := range jobs {
		if j.Unit == v.Unit && j.State == "queued" {
			if _, e = s.Sync(ctx, j.Unit, j.ID); e != nil {
				return v, e
			}
		}
	}
	s.recordPresetError(ctx, nil)
	return s.CodexStatus(ctx)
}
