package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIndependentSourceSchedules(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	network := AutoPolicy{AwaySmall: true}
	for _, p := range []string{"manual", "home"} {
		if _, err = f.s.SetSchedule(ctx, b.Unit, p, 15); err != nil {
			t.Fatal(err)
		}
		snap, err := f.s.CaptureDue(ctx, b, network)
		if err != nil || snap != nil {
			t.Fatal("unexpected capture", p, snap, err)
		}
		v, err := f.s.Snapshot(ctx, b.Unit, "manual")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.AutoSync(ctx, v, network); !errors.Is(err, ErrWaiting) {
			t.Fatal("unexpected transfer", p, err)
		}
	}
	if _, err = f.s.SetSchedule(ctx, b.Unit, "network", 120); err != nil {
		t.Fatal(err)
	}
	// Explicit snapshots above already represent this empty directory.
	snap, completed, err := f.s.CaptureScheduled(ctx, b, network)
	if err != nil || !completed || snap != nil {
		t.Fatal("unchanged scan", snap, completed, err)
	}
	snap, completed, err = f.s.CaptureScheduled(ctx, b, network)
	if err != nil || completed || snap != nil {
		t.Fatal("interval", snap, completed, err)
	}
	if _, err = f.s.SetSchedule(ctx, b.Unit, "network", 0); err == nil {
		t.Fatal("invalid interval accepted")
	}
}

func TestCodexPresetLifecycle(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	if err := os.WriteFile(filepath.Join(root, "history.jsonl"), []byte("{\"example\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := f.nas.AddUnit("other")
	if err != nil {
		t.Fatal(err)
	}
	var dev string
	if err = f.s.db.QueryRow("SELECT device FROM account").Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if err = f.nas.Grant(second, dev, "sync"); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.s.CodexStatus(ctx)
	if err != nil || cfg.Enabled || cfg.Unit != "" {
		t.Fatal(cfg, err)
	}
	if _, err = f.s.EnableCodex(ctx, "", "network", 30); err == nil {
		t.Fatal("ambiguous library")
	}
	cfg, err = f.s.EnableCodex(ctx, f.unit, "manual", 30)
	if err != nil || cfg.Scope != f.unit || !cfg.Enabled {
		t.Fatal(cfg, err)
	}
	if _, err = f.s.EnableCodex(ctx, second, "home", 30); err == nil {
		t.Fatal("target changed without re-enrollment")
	}
	b, err := f.s.binding(ctx, cfg.Unit)
	if err != nil || b.Profile != "codex" {
		t.Fatal(b, err)
	}
	snap, err := f.s.CaptureDue(ctx, b, AutoPolicy{AwaySmall: true})
	if snap != nil || err != nil {
		t.Fatal(snap, err)
	}
	cfg, err = f.s.RunCodex(ctx)
	if err != nil || cfg.LastBackup == "" {
		t.Fatal(cfg, err)
	}
	if _, err = f.s.DisableCodex(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Snapshot(ctx, cfg.Unit, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.AutoSync(ctx, v, AutoPolicy{AwaySmall: true}); !errors.Is(err, ErrWaiting) {
		t.Fatal("disabled transfer", err)
	}
	cfg, err = f.s.RunCodex(ctx)
	if err != nil || cfg.Enabled {
		t.Fatal("explicit backup", cfg, err)
	}
}
