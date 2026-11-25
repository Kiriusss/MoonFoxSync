package engine

import (
	"context"
	"errors"
	"net/url"
)

type MergePreview struct {
	Kind      string `json:"kind"`
	Size      *int64 `json:"size,omitempty"`
	Text      string `json:"text"`
	Target    string `json:"target,omitempty"`
	Truncated bool   `json:"truncated"`
}

func (r *Remote) MergeRead(ctx context.Context, unit, snap, p string) (MergePreview, error) {
	var out MergePreview
	for _, id := range []string{unit, snap} {
		if e := validID(id); e != nil {
			return out, e
		}
	}
	if p == "" {
		return out, errors.New("请选择版本中的文件")
	}
	err := r.call(ctx, "GET", "/units/"+unit+"/merge/preview", url.Values{"snapshotId": {snap}, "path": {p}}, nil, &out)
	return out, err
}
