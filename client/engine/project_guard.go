package engine

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"kirius.local/kiriusdrive/internal/objectfmt"
)

var fingerprintQuery = url.Values{"fingerprint": {"true"}}

// Fingerprint exactly the files the Git capture will include. Eagerly checking an
// existing clone at bind time allows later legitimate edits even before the
// first automatic scan. Delay is local disk I/O, never a download of file bodies.
func (s *Store) baselineFingerprint(ctx context.Context, b Binding, policy IgnoredPolicy) (string, error) {
	dir, name := b.Path, "."
	if b.Profile == "file" {
		dir, name = filepath.Dir(b.Path), filepath.Base(b.Path)
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		return "", e
	}
	defer root.Close()
	c := capture{ctx: ctx, root: root, timeless: true, store: s}
	if err := c.prepareArchives(b); err != nil {
		return "", err
	}
	if b.Profile == "project" {
		if _, e = c.projectMembers(dir, policy); e != nil {
			return "", e
		}
	}
	entry, e := c.entry(name)
	if e != nil {
		return "", e
	}
	if err := c.checkArchiveCoverage(); err != nil {
		return "", err
	}
	for _, check := range c.checks {
		stat, err := root.Lstat(check.path)
		if err != nil || !sameFile(stat, check.info) {
			return "", errors.New("基线核对期间目录有变化，请重试绑定")
		}
	}
	if b.Profile == "file" {
		return objectfmt.BuildDir([]objectfmt.Entry{entry}, c.put)
	}
	return entry.Digest, nil
}
