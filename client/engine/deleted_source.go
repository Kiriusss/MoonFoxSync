package engine

import (
	"context"
	"errors"
	"fmt"
)

var errLocalFilesDeleted = errors.New("本机文件删除后已暂停此来源的同步。可从版本历史恢复文件，或为项目接收 NAS 版本")

// Call under an exclusive project lock before moving the first file. The pause
// belongs to this binding generation, survives restarts and leaves immutable
// snapshots (including pending ones) intact.
func (s *Store) pauseLocalDeletion(ctx context.Context, b Binding) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO local_deleted_sources(unit,generation)
		SELECT unit,generation FROM bindings WHERE unit=? AND generation=? AND path=? AND profile IN('project','file')
		ON CONFLICT(unit) DO UPDATE SET generation=excluded.generation`, b.Unit, b.generation, b.Path)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("本机来源绑定已经变化，请重新选择项目")
	}
	return err
}

func (s *Store) localDeletionPaused(ctx context.Context, unit string) (bool, error) {
	var paused bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM local_deleted_sources d JOIN bindings b ON b.unit=d.unit AND b.generation=d.generation WHERE b.unit=? AND b.path IS NOT NULL)`, unit).Scan(&paused)
	return paused, err
}

// The deliberate confirm step of a same-path bind can resume only the revision
// that was previewed. Automatic imports and preview calls never reach this path.
func (s *Store) resumeRestoredBinding(ctx context.Context, previous Binding, expected string) (Binding, error) {
	locks, err := s.projectActivity(previous.Unit)
	if err != nil {
		return previous, err
	}
	defer locks.Close()
	b, err := s.binding(ctx, previous.Unit)
	if err != nil {
		return b, err
	}
	if !b.LocalFilesDeleted || b.generation != previous.generation || b.Path != previous.Path || expected == "" || b.Base == nil || *b.Base != expected {
		return b, errors.New("请重新预览并确认已恢复的原位置和版本")
	}
	// A second delete may have started in the short interval between the
	// original bind check and acquiring this lock.
	if err = s.guardWorkspace(ctx, b.Unit); err != nil {
		return b, err
	}
	remote, err := s.Remote(ctx)
	if err != nil {
		return b, err
	}
	var version checkoutRecord
	if err = remote.call(ctx, "GET", "/units/"+b.Unit+"/snapshots/"+expected, fingerprintQuery, nil, &version); err != nil {
		return b, fmt.Errorf("无法核对选定版本。若有待传快照，可先手动上传该时间点：%w", err)
	}
	if version.Unit != b.Unit || version.Snapshot != expected || len(version.ContentDigest) != 64 {
		return b, errors.New("NAS 版本与所选来源不一致")
	}
	policy, err := readIgnored(ctx, s.db, b.Unit)
	if err != nil {
		return b, err
	}
	digest, err := s.baselineFingerprint(ctx, b, policy)
	if err != nil {
		return b, err
	}
	if digest != version.ContentDigest {
		return b, errors.New("原位置内容与已备份版本不同，来源保持暂停。请先恢复完整内容，再确认绑定")
	}
	marker, err := directoryMarkerAt(boundDirectory(b.Path, b.Profile))
	if err != nil {
		return b, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM local_deleted_sources WHERE unit=? AND generation=? AND EXISTS(
		SELECT 1 FROM bindings b JOIN binding_roots r ON r.unit=b.unit WHERE b.unit=? AND b.path=? AND b.generation=? AND b.base=? AND r.identity=?)`,
		b.Unit, b.generation, b.Unit, b.Path, b.generation, expected, marker)
	if err != nil {
		return b, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return b, err
	}
	if n != 1 {
		return b, errors.New("核对期间本机来源发生变化，请重新预览")
	}
	b.LocalFilesDeleted = false
	return b, nil
}
