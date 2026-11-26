package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

type WorkspaceFile struct {
	Slot       string          `json:"slot,omitempty"`
	Unit       string          `json:"unitId"`
	Snapshot   string          `json:"snapshotId"`
	Member     string          `json:"path"`
	Source     string          `json:"sourcePath"`
	SHA        string          `json:"sha256"`
	Size       int64           `json:"size"`
	Device     string          `json:"-"`
	State      string          `json:"state"`
	Generation string          `json:"-"`
	Entry      objectfmt.Entry `json:"-"`
	Directory  string          `json:"-"`
}

const workspaceColumns = "slot,unit,snapshot,member,source_path,sha,size,device,phase,generation,entry,dir_id"

func readWorkspace(row interface{ Scan(...any) error }) (WorkspaceFile, error) {
	var a WorkspaceFile
	var raw []byte
	err := row.Scan(&a.Slot, &a.Unit, &a.Snapshot, &a.Member, &a.Source, &a.SHA, &a.Size, &a.Device, &a.State, &a.Generation, &raw, &a.Directory)
	if err == nil {
		err = json.Unmarshal(raw, &a.Entry)
	}
	return a, err
}
func (s *Store) workspaceTarget(a WorkspaceFile) string {
	return filepath.Join(s.Home, "workspace-volume", a.Slot, filepath.Base(a.Member))
}
func (s *Store) listWorkspace(ctx context.Context, unit string) ([]WorkspaceFile, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+workspaceColumns+" FROM workspace_archives WHERE (?='' OR unit=?) ORDER BY source_path", unit, unit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []WorkspaceFile{}
	for rows.Next() {
		a, e := readWorkspace(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, a)
	}
	return items, rows.Err()
}
func (s *Store) WorkspaceFiles(ctx context.Context) ([]WorkspaceFile, error) {
	return s.listWorkspace(ctx, "")
}
func (s *Store) guardWorkspace(ctx context.Context, unit string) error {
	var any bool
	if e := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM local_delete_intents WHERE ?1='' OR unit=?1)", unit).Scan(&any); e != nil {
		return e
	}
	if any {
		return errors.New("本机文件删除尚未完成，请先在原项目重试「删除本机文件」")
	}
	if e := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM workspace_archives WHERE ?='' OR unit=?)", unit, unit).Scan(&any); e != nil {
		return e
	}
	if any {
		return errors.New("此操作会解除原位置按需文件的关联，请先在「原位置按需」中取回这些文件")
	}
	return nil
}

// Inspect the references relative to the same open root used by the capture.
// The mounted volume may be offline. Only the identity and target of the
// symlink itself matter; data and metadata come from the published file.
func (c *capture) prepareArchives(b Binding) error {
	items, err := c.store.listWorkspace(c.ctx, b.Unit)
	if err != nil || len(items) == 0 {
		return err
	}
	var device string
	if err = c.store.db.QueryRowContext(c.ctx, "SELECT device FROM account WHERE id=1").Scan(&device); err != nil {
		return err
	}
	c.archives = make(map[string]WorkspaceFile, len(items))
	base := boundDirectory(b.Path, b.Profile)
	parents := make(map[string]string)
	for _, a := range items {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if a.State != "online" || a.Generation != b.generation || a.Device != device || validID(a.Slot) != nil {
			return fmt.Errorf("原位置按需记录与本次采集不一致，请先检查或取回 %s", a.Source)
		}
		rel := filepath.FromSlash(a.Member)
		if !validMember(a.Member) || a.Source != filepath.Join(base, rel) || b.Profile == "file" && a.Source != b.Path ||
			a.Entry.Kind != "file" || a.Entry.Name != filepath.Base(rel) {
			return fmt.Errorf("原位置按需记录的文件位置不一致：%s", a.Member)
		}
		parent := filepath.Dir(rel)
		marker, known := parents[parent]
		if !known {
			// Never walk through a symlinked ancestor or a newly nested repository.
			if _, err := ignoredRegular(c.root, a.Member); err != nil {
				return fmt.Errorf("按需文件已移动或所在目录已更改：%s: %w", a.Source, err)
			}
			dir, err := c.root.Open(parent)
			if err != nil {
				return err
			}
			marker, err = directoryMarker(dir)
			info, statErr := dir.Stat()
			dir.Close()
			if err != nil {
				return err
			}
			if statErr != nil {
				return statErr
			}
			parents[parent] = marker
			c.checks = append(c.checks, statCheck{parent, info})
		}
		if marker != a.Directory {
			return fmt.Errorf("按需文件的上级目录已被替换：%s", a.Source)
		}
		info, err := c.root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("原位置按需文件缺失：%s: %w", a.Source, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("原位置已被其他文件占用：%s", a.Source)
		}
		target, err := c.root.Readlink(rel)
		if err != nil || target != c.store.workspaceTarget(a) {
			return fmt.Errorf("原位置按需链接已被替换：%s", a.Source)
		}
		c.checks = append(c.checks, statCheck{rel, info})
		c.archives[a.Member] = a
	}
	return nil
}

// A directory may become a nested repository while Git or the filesystem is
// being enumerated. Every selected archive must have appeared in the tree.
func (c *capture) checkArchiveCoverage() error {
	for member := range c.archives {
		p := filepath.FromSlash(member)
		if !c.excluded[p] && (c.included == nil || c.included[p]) {
			return fmt.Errorf("原位置按需文件未出现在采集结果中，请检查其所在目录：%s", member)
		}
	}
	return nil
}
func (c *capture) archivedEntry(a WorkspaceFile) (objectfmt.Entry, error) {
	if c.timeless {
		entry := a.Entry
		entry.MtimeNS = 0
		return entry, nil
	}
	info, e := objectfmt.ReadFile(a.Entry.Digest, func(ref objectfmt.Ref) ([]byte, error) {
		b, err := c.store.load(ref)
		if err == nil {
			c.refs[ref] = len(b)
		}
		return b, err
	})
	if e != nil {
		return objectfmt.Entry{}, e
	}
	if info.SHA256 != a.SHA || info.Size != a.Size {
		return objectfmt.Entry{}, errors.New("按需文件的内容描述已变化")
	}
	remaining := info.Size
	for _, ref := range info.Chunks {
		sz := min(int64(objectfmt.ChunkSize), remaining)
		c.refs[ref] = int(sz)
		remaining -= sz
	}
	return a.Entry, nil
}
func (s *Store) sourceRecord(ctx context.Context, source string) (WorkspaceFile, error) {
	name, err := canonicalWorkspacePath(source)
	if err != nil {
		return WorkspaceFile{}, err
	}
	a, err := readWorkspace(s.db.QueryRowContext(ctx, "SELECT "+workspaceColumns+" FROM workspace_archives WHERE source_path=?", name))
	if err != nil {
		return WorkspaceFile{}, err
	}
	var bindingPath, profile string
	err = s.db.QueryRowContext(ctx, `SELECT b.path,b.profile FROM bindings b JOIN account d ON d.id=1
		WHERE b.unit=? AND b.path IS NOT NULL AND b.generation=? AND d.device=?`, a.Unit, a.Generation, a.Device).Scan(&bindingPath, &profile)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkspaceFile{}, errors.New("按需记录与当前绑定或设备不一致")
	}
	if err != nil {
		return WorkspaceFile{}, err
	}
	if !validMember(a.Member) || filepath.Join(boundDirectory(bindingPath, profile), filepath.FromSlash(a.Member)) != name ||
		(profile == "file" && bindingPath != name) || (profile != "project" && profile != "file") {
		return WorkspaceFile{}, errors.New("按需记录与当前绑定路径不一致")
	}
	return a, nil
}
func (s *Store) requireWorkspaceHold(ctx context.Context, a WorkspaceFile) error {
	r, e := s.Remote(ctx)
	if e != nil {
		return e
	}
	var h struct {
		Unit     string `json:"unitId"`
		Snapshot string `json:"snapshotId"`
		Path     string `json:"path"`
		Device   string `json:"deviceId"`
		ID       string `json:"holdId"`
		State    string `json:"state"`
	}
	e = r.call(ctx, "PUT", "/units/"+a.Unit+"/holds/"+a.Slot, nil, map[string]string{"snapshotId": a.Snapshot, "path": a.Member}, &h)
	if e != nil {
		return e
	}
	if h.Unit != a.Unit || h.Snapshot != a.Snapshot || h.Path != a.Member || h.Device != a.Device || h.ID != a.Slot || h.State != "active" {
		return errors.New("NAS 未确认保留当前文件版本")
	}
	return nil
}
