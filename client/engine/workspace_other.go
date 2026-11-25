//go:build !darwin || !cgo

package engine

import (
	"context"
	"errors"
)

func (s *Store) WorkspaceRelease(context.Context, string, string, string) (WorkspaceFile, error) {
	return WorkspaceFile{}, errors.New("请使用 macOS 客户端的原位置按需功能")
}
func (s *Store) WorkspaceHydrate(context.Context, string) (WorkspaceFile, error) {
	return WorkspaceFile{}, errors.New("请使用 macOS 客户端的原位置按需功能")
}
