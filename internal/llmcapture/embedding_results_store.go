package llmcapture

import (
	"context"
	"fmt"
)

func (s *PostgresStore) RecheckEmbeddingRequest(ctx context.Context, key, infoHash []byte) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("embedding admission store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	var admitted bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (`+resultPublicAdmissionSQL+`
  AND a.info_hash = $2 AND c.task = 'matcher_embedding')`, key, infoHash).Scan(&admitted)
	if err != nil {
		return err
	}
	if !admitted {
		return fmt.Errorf("embedding capture is absent, mismatched, expired or no longer public")
	}
	return nil
}
