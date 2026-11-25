package engine

import (
	"context"
	"errors"
)

type TransferSettings struct {
	Uploads   int `json:"uploads"`
	Downloads int `json:"downloads"`
}

const defaultFileTransfers = 4
const maxFileTransfers = 50

func (s *Store) TransferSettings(ctx context.Context) (TransferSettings, error) {
	var v TransferSettings
	err := s.db.QueryRowContext(ctx, "SELECT uploads,downloads FROM transfer_settings WHERE id=1").Scan(&v.Uploads, &v.Downloads)
	return v, err
}

func (s *Store) SetTransferSettings(ctx context.Context, uploads, downloads *int) (TransferSettings, error) {
	for _, n := range []*int{uploads, downloads} {
		if n != nil && (*n < 1 || *n > maxFileTransfers) {
			return TransferSettings{}, errors.New("并行文件数须为 1 至 50")
		}
	}
	var v TransferSettings
	err := s.db.QueryRowContext(ctx, "UPDATE transfer_settings SET uploads=COALESCE(?,uploads),downloads=COALESCE(?,downloads) WHERE id=1 RETURNING uploads,downloads", uploads, downloads).Scan(&v.Uploads, &v.Downloads)
	return v, err
}
