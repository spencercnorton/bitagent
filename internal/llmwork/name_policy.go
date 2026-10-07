package llmwork

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

func (s *Store) nameAdmission(ctx context.Context, tx pgx.Tx, hash []byte, stage string) error {
	if !s.namePolicy.Enabled() {
		return nil
	}
	var name, kind string
	if err := tx.QueryRow(ctx, `SELECT t.name,CASE WHEN EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=t.info_hash AND c.content_type='xxx') THEN 'xxx' ELSE '' END FROM torrents t WHERE t.info_hash=$1 FOR SHARE`, hash).Scan(&name, &kind); err != nil {
		return ErrHeld
	}
	var id protocol.ID
	copy(id[:], hash)
	d := s.namePolicy.EvaluateClassified(id, name, kind)
	if !d.Eligible {
		s.namePolicy.Observe(stage, d)
		return ErrHeld
	}
	return nil
}
