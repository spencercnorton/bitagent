package cataloguerecovery

import (
	"context"
	"time"
)

type ClassifierDelete struct {
	ID        int64     `json:"id"`
	InfoHash  []byte    `json:"info_hash"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Store) ListClassifierDeletes(ctx context.Context, limit, offset int) ([]ClassifierDelete, int, error) {
	if !s.cfg.Enabled {
		return nil, 0, ErrDisabled
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM catalogue_recovery_snapshots s JOIN torrent_verdict_events e ON e.id = s.removed_verdict_event_id WHERE s.state = 'removed' AND e.mechanism = 'classifier_delete'`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT s.id, s.info_hash, s.reason, s.created_at, s.expires_at FROM catalogue_recovery_snapshots s JOIN torrent_verdict_events e ON e.id = s.removed_verdict_event_id WHERE s.state = 'removed' AND e.mechanism = 'classifier_delete' ORDER BY s.id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []ClassifierDelete
	for rows.Next() {
		var d ClassifierDelete
		if err := rows.Scan(&d.ID, &d.InfoHash, &d.Reason, &d.CreatedAt, &d.ExpiresAt); err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}
