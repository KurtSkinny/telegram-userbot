package peersmgr

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"go.etcd.io/bbolt"
)

// WarmupIfEmpty выполняет прогрев: при пустой БД загружает диалоги, применяет их к менеджеру
// и сохраняет снимок/пиры для дальнейшей работы офлайн.
func (s *Service) WarmupIfEmpty(ctx context.Context, api *tg.Client) error {
	empty, err := s.isDatabaseEmpty()
	if err != nil {
		return fmt.Errorf("peersmgr: check db empty: %w", err)
	}
	if !empty {
		return nil
	}
	return s.RefreshDialogs(ctx, api)
}

func (s *Service) isDatabaseEmpty() (bool, error) {
	empty := true
	err := s.db.View(func(tx *bbolt.Tx) error {
		if bucket := tx.Bucket(peersBucketBytes); bucket != nil {
			cursor := bucket.Cursor()
			key, _ := cursor.First()
			if key != nil {
				empty = false
				return nil
			}
		}
		if bucket := tx.Bucket(dialogsSnapshotBuckets); bucket != nil {
			value := bucket.Get(dialogsSnapshotKeyBytes)
			if len(value) > 0 {
				empty = false
			}
		}
		return nil
	})
	return empty, err
}
