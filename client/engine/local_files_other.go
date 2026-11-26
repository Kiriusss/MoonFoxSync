//go:build !darwin || !cgo

package engine

import (
	"context"
	"errors"
)

func (s *Store) DeleteLocalFiles(context.Context, string, string) (LocalFileDeleteResult, error) {
	return LocalFileDeleteResult{}, errors.New("请使用 macOS 客户端删除本机文件")
}
