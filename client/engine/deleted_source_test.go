package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDeletedSourceBlocksCaptureAndScheduledUpload(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	makeFile(t, f.source, "report.txt", "published")
	_ = publishedWorkspaceCapture(t, f)
	makeFile(t, f.source, "report.txt", "pending edit")
	job, err := f.s.Snapshot(ctx, f.unit, "earlier edit")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.pauseLocalDeletion(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(f.source, "report.txt")); err != nil {
		t.Fatal(err)
	}
	s, err := Open(f.s.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Snapshot(ctx, f.unit, "manual"); !errors.Is(err, errLocalFilesDeleted) {
		t.Fatalf("manual capture: %v", err)
	}
	if v, e := s.CaptureDue(ctx, b, AutoPolicy{}); v != nil || e != nil {
		t.Fatalf("old scheduler binding: %v %v", v, e)
	}
	if _, err = s.AutoSync(ctx, job, AutoPolicy{AwaySmall: true}); !errors.Is(err, ErrWaiting) {
		t.Fatalf("auto upload: %v", err)
	}
	res, err := s.SyncPending(ctx, AutoPolicy{})
	if err != nil || res.Skipped != 1 || res.Total != 1 || res.Uploaded != 0 || res.Failed != 0 {
		t.Fatalf("batch: %+v %v", res, err)
	}
	remote, err := s.Remote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h, err := remote.History(ctx, f.unit)
	if err != nil || len(h.Items) != 1 {
		t.Fatalf("unexpected NAS history: %d %v", len(h.Items), err)
	}
	pr, err := s.ProjectStates(ctx, false)
	if err != nil || len(pr.Items) != 1 || pr.Items[0].NAS != "localFilesDeleted" {
		t.Fatalf("status: %+v %v", pr, err)
	}
	if _, err = s.Sync(ctx, f.unit, job.ID); err != nil {
		t.Fatalf("explicit publish of previously captured version: %v", err)
	}
	var stillPaused bool
	if stillPaused, err = s.localDeletionPaused(ctx, f.unit); err != nil || !stillPaused {
		t.Fatalf("explicit publish resumed capture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "checked-out")
	newBinding, err := s.CheckoutProject(ctx, f.unit, dst)
	if err != nil || newBinding.LocalFilesDeleted {
		t.Fatalf("checked-out binding: %+v %v", newBinding, err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "report.txt"))
	if err != nil || string(data) != "pending edit" {
		t.Fatalf("restored bytes: %q %v", data, err)
	}
	if _, err = s.Snapshot(ctx, f.unit, "after checkout"); err != nil {
		t.Fatalf("capture in restored location: %v", err)
	}
}

func TestDeletedSingleFileResumesOnlyOnVerifiedExplicitBind(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var device string
	if err := f.s.db.QueryRow("SELECT device FROM account").Scan(&device); err != nil {
		t.Fatal(err)
	}
	unit, err := f.nas.AddUnit("single")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.nas.Grant(unit, device, "sync"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	file := filepath.Join(root, "source.txt")
	makeFile(t, root, "source.txt", "saved")
	if _, err = f.s.Bind(ctx, unit, file, "file", false, ""); err != nil {
		t.Fatal(err)
	}
	local := f
	local.unit = unit
	v := publishedWorkspaceCapture(t, local)
	b, err := f.s.binding(ctx, unit)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.pauseLocalDeletion(ctx, b); err != nil {
		t.Fatal(err)
	}
	makeFile(t, root, "source.txt", "other")
	if _, err = f.s.Bind(ctx, unit, file, "file", false, v.ID); err == nil {
		t.Fatal("resumed with different contents")
	}
	makeFile(t, root, "source.txt", "saved")
	for _, confirm := range []bool{false, true} {
		preview, err := f.s.Bind(ctx, unit, file, "file", true, v.ID)
		if err != nil || !preview.LocalFilesDeleted {
			t.Fatalf("preview unpaused: %+v %v", preview, err)
		}
		if !confirm {
			existing, err := f.s.bindSource(ctx, unit, file, "file", false, v.ID)
			if err != nil || !existing.LocalFilesDeleted {
				t.Fatalf("implicit import unpaused: %+v %v", existing, err)
			}
		}
	}
	active, err := f.s.Bind(ctx, unit, file, "file", false, v.ID)
	if err != nil || active.LocalFilesDeleted {
		t.Fatalf("restore bind: %+v %v", active, err)
	}
	if _, err = f.s.Snapshot(ctx, unit, "restored"); err != nil {
		t.Fatal(err)
	}
}

func TestDeletedSourceForgetPreservesInterruptedIntent(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	b, err := f.s.binding(ctx, f.unit)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.pauseLocalDeletion(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec("INSERT INTO local_delete_intents VALUES(?,?,?)", b.Unit, b.generation, []byte("pending")); err != nil {
		t.Fatal(err)
	}
	plan, err := f.s.LocalRemovalPlan(ctx, b.Unit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RemoveLocal(ctx, b.Unit, plan.Token); err == nil {
		t.Fatal("forgot source during unfinished delete")
	}
	if _, err = f.s.db.Exec("DELETE FROM local_delete_intents WHERE unit=?", b.Unit); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RemoveLocal(ctx, b.Unit, plan.Token); err != nil {
		t.Fatal(err)
	}
	if paused, e := f.s.localDeletionPaused(ctx, b.Unit); e != nil || paused {
		t.Fatalf("stale pause: %v", e)
	}
}
