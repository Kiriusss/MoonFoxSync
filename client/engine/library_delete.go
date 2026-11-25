package engine

import (
	"context"
	"errors"
)

type LibraryDeletion struct {
	Name      string `json:"name"`
	Units     int    `json:"units"`
	Snapshots int    `json:"snapshots"`
	Devices   int    `json:"devices"`
	Holds     int    `json:"holds"`
	Uploads   int    `json:"uploads"`
	Token     string `json:"token"`
}

func (r *Remote) LibraryDeletePlan(ctx context.Context, unit string) (LibraryDeletion, error) {
	var p LibraryDeletion
	if err := validID(unit); err != nil {
		return p, err
	}
	err := r.call(ctx, "GET", "/units/"+unit+"/delete-plan", nil, nil, &p)
	return p, err
}
func (r *Remote) DeleteLibrary(ctx context.Context, unit, name, token string) (map[string]any, error) {
	if err := validID(unit); err != nil {
		return nil, err
	}
	if name == "" || token == "" {
		return nil, errors.New("请先预览并输入资料库名称")
	}
	var out map[string]any
	err := r.commitCall(ctx, "POST", "/units/"+unit+"/delete", map[string]string{"name": name, "token": token}, &out)
	return out, err
}
