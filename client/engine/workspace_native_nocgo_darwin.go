//go:build darwin && !cgo

package engine

import (
	"errors"
	"os"
)

var errWorkspaceNativeUnavailable = errors.New("原位置按需文件需要支持 macOS 文件保护的客户端构建")

func workspaceHeldByAnother(string) (bool, error)    { return false, errWorkspaceNativeUnavailable }
func workspaceCopyMetadata(*os.File, *os.File) error { return errWorkspaceNativeUnavailable }
