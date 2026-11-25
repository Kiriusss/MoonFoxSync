package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"kirius.local/kiriusdrive/client/engine"
)

func providerCommand(ctx context.Context, s *engine.Store, args []string) (any, error) {
	if len(args) != 1 || args[0] != "webdav" {
		return nil, errors.New("使用 provider webdav")
	}
	return nil, s.ServeWorkspaceVolume(ctx, func(ready engine.WorkspaceVolumeReady) error { return json.NewEncoder(os.Stdout).Encode(ready) })
}
