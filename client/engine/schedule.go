package engine

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Each source inherits the device network limits unless explicitly restricted.
type SourceSchedule struct {
	Unit     string `json:"unitId"`
	Policy   string `json:"policy"`
	Minutes  int    `json:"intervalMinutes"`
	lastScan int64
}

func validateSchedule(policy string, minutes int) error {
	if policy != "network" && policy != "home" && policy != "manual" {
		return errors.New("请选择跟随网络、仅家庭网络或仅手动")
	}
	if minutes < 1 || minutes > 1440 {
		return errors.New("检查间隔须为 1 至 1440 分钟")
	}
	return nil
}
func (s *Store) Schedule(ctx context.Context, unit string) (SourceSchedule, error) {
	v := SourceSchedule{Unit: unit, Policy: "network", Minutes: 1}
	if _, e := s.binding(ctx, unit); e != nil {
		return v, e
	}
	e := s.db.QueryRowContext(ctx, "SELECT policy,minutes,last_scan FROM schedules WHERE unit=?", unit).Scan(&v.Policy, &v.Minutes, &v.lastScan)
	if errors.Is(e, sql.ErrNoRows) {
		return v, nil
	}
	return v, e
}

const writeSchedule = `INSERT INTO schedules(unit,policy,minutes) VALUES(?,?,?) ON CONFLICT(unit) DO UPDATE SET policy=excluded.policy,minutes=excluded.minutes,last_scan=0`

func (s *Store) SetSchedule(ctx context.Context, unit, policy string, minutes int) (SourceSchedule, error) {
	var v SourceSchedule
	if e := validateSchedule(policy, minutes); e != nil {
		return v, e
	}
	b, e := s.binding(ctx, unit)
	if e != nil {
		return v, e
	}
	if b.Profile == "codex" {
		return v, errors.New("请在 Codex 对话历史设置中调整策略")
	}
	if _, e = s.db.ExecContext(ctx, writeSchedule, unit, policy, minutes); e != nil {
		return v, e
	}
	return s.Schedule(ctx, unit)
}

// Reserve the scan before touching the filesystem so concurrent resident processes
// share one interval. Failed captures will be retried at the next interval.
func (s *Store) scanDue(ctx context.Context, b Binding, p AutoPolicy) (bool, error) {
	cfg, e := s.Schedule(ctx, b.Unit)
	if e != nil {
		return false, e
	}
	now := time.Now().Unix()
	if cfg.Policy == "manual" || (cfg.lastScan <= now && now-cfg.lastScan < int64(cfg.Minutes)*60) {
		return false, nil
	}
	if cfg.Policy == "home" {
		if p.HomeURL == "" {
			return false, nil
		}
		if _, e = s.homeRemote(ctx, p); e != nil {
			if errors.Is(e, ErrWaiting) {
				return false, nil
			}
			return false, e
		}
	}
	_, e = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO schedules(unit,policy,minutes) VALUES(?,?,?)", b.Unit, cfg.Policy, cfg.Minutes)
	if e != nil {
		return false, e
	}
	rr, e := s.db.ExecContext(ctx, "UPDATE schedules SET last_scan=? WHERE unit=? AND last_scan=? AND minutes=? AND policy=?", now, b.Unit, cfg.lastScan, cfg.Minutes, cfg.Policy)
	if e != nil {
		return false, e
	}
	n, e := rr.RowsAffected()
	return n == 1, e
}
func (s *Store) CaptureDue(ctx context.Context, b Binding, p AutoPolicy) (*Snapshot, error) {
	if b.LocalFilesDeleted {
		return nil, nil
	}
	if b.Profile == "codex" {
		lock, e := s.presetLock()
		if e != nil {
			return nil, e
		}
		defer lock.Close()
		status, e := s.CodexStatus(ctx)
		if e != nil || !status.Enabled {
			return nil, e
		}
	}
	due, e := s.scanDue(ctx, b, p)
	if e != nil || !due {
		return nil, e
	}
	v, e := s.Snapshot(ctx, b.Unit, "")
	if errors.Is(e, errLocalFilesDeleted) {
		return nil, nil
	}
	if b.Profile == "codex" {
		s.recordPresetError(ctx, e)
	}
	return &v, e
}
