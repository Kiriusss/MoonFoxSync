package engine

import (
	"context"
	"errors"
)

type HistoryPolicy struct {
	Mode  string `json:"mode"`
	Value int    `json:"value"`
}
type PolicyPreview struct {
	HistoryPolicy
	Remove    int `json:"removeCount"`
	Protected int `json:"protectedCount"`
}
type RebuiltPoint struct {
	Snapshot string   `json:"snapshotId"`
	Label    string   `json:"label"`
	Before   []string `json:"parentsBefore"`
	After    []string `json:"parentsAfter"`
}
type DeletePreview struct {
	Snapshot  string         `json:"snapshotId"`
	Head      string         `json:"expectedHead"`
	Token     string         `json:"planToken"`
	Protected string         `json:"protectedReason,omitempty"`
	Points    []RebuiltPoint `json:"rebuiltPoints"`
}
type DeletedPoint struct {
	Points   []RebuiltPoint `json:"rebuiltPoints"`
	Snapshot string         `json:"snapshotId"`
	Deleted  bool           `json:"deleted"`
}

func (p HistoryPolicy) validate() error {
	switch p.Mode {
	case "all":
		if p.Value == 0 {
			return nil
		}
	case "latest":
		if p.Value >= 1 && p.Value <= 10000 {
			return nil
		}
	case "days":
		if p.Value >= 1 && p.Value <= 3650 {
			return nil
		}
	}
	return errors.New("保留全部时取值为 0，可保留最近 1–10000 个时间点或最近 1–3650 天")
}
func (r *Remote) HistoryPolicy(ctx context.Context, unit string) (HistoryPolicy, error) {
	var out HistoryPolicy
	if e := validID(unit); e != nil {
		return out, e
	}
	err := r.call(ctx, "GET", "/units/"+unit+"/history-policy", nil, nil, &out)
	return out, err
}
func (r *Remote) PreviewPolicy(ctx context.Context, unit string, p HistoryPolicy) (PolicyPreview, error) {
	var out PolicyPreview
	if e := validID(unit); e != nil {
		return out, e
	}
	if e := p.validate(); e != nil {
		return out, e
	}
	err := r.call(ctx, "POST", "/units/"+unit+"/history-policy/preview", nil, p, &out)
	return out, err
}
func (r *Remote) SetHistoryPolicy(ctx context.Context, unit string, p HistoryPolicy, head string) (HistoryPolicy, error) {
	var out HistoryPolicy
	if e := validID(unit); e != nil {
		return out, e
	}
	if e := p.validate(); e != nil {
		return out, e
	}
	body := map[string]any{"mode": p.Mode, "value": p.Value}
	if head != "" {
		if e := validID(head); e != nil {
			return out, e
		}
		body["expectedHead"] = head
	}
	err := r.commitCall(ctx, "PUT", "/units/"+unit+"/history-policy", body, &out)
	return out, err
}
func (r *Remote) PreviewDelete(ctx context.Context, unit, snap, head string) (DeletePreview, error) {
	var out DeletePreview
	for _, id := range []string{unit, snap, head} {
		if err := validID(id); err != nil {
			return out, err
		}
	}
	err := r.call(ctx, "POST", "/units/"+unit+"/timeline/delete/preview", nil, map[string]string{"snapshotId": snap, "expectedHead": head}, &out)
	return out, err
}
func (r *Remote) DeletePoint(ctx context.Context, unit, snap, head, token string) (DeletedPoint, error) {
	var out DeletedPoint
	for _, id := range []string{unit, snap, head} {
		if e := validID(id); e != nil {
			return out, e
		}
	}
	if token == "" {
		return out, errors.New("请先预览删除计划并提供 --plan-token")
	}
	err := r.commitCall(ctx, "POST", "/units/"+unit+"/timeline/delete", map[string]string{"snapshotId": snap, "expectedHead": head, "planToken": token}, &out)
	if err == nil && (!out.Deleted || out.Snapshot != snap) {
		return out, errors.New("NAS 返回的删除回执与选中的时间点不符")
	}
	return out, err
}
func (r *Remote) RestorePoint(ctx context.Context, unit, snap, head, request, label string) (Receipt, error) {
	var out Receipt
	for _, id := range []string{unit, snap, head, request} {
		if e := validID(id); e != nil {
			return out, e
		}
	}
	err := r.commitCall(ctx, "POST", "/units/"+unit+"/timeline/restore", map[string]string{"snapshotId": snap, "expectedHead": head, "requestId": request, "label": label}, &out)
	if err == nil && (out.Unit != unit || out.Outcome != "published" || validID(out.Snapshot) != nil) {
		return out, errors.New("NAS 恢复回执与所选项目不符")
	}
	return out, err
}
