package junkpurge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/verdicts"
)

// recordQuarantineVerdict observes the current snapshot under the same
// snapshot-before-ledger lock order as expiry and operator actions. Admission
// bookkeeping currently calls it after its transaction; a delayed observation
// must not overwrite an already restored, deleted or expired transition.
func recordQuarantineVerdict(ctx context.Context, pool *pgxpool.Pool, hash []byte, days int, reason string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var quarantinedAt time.Time
	var expiredAt *time.Time
	err = tx.QueryRow(ctx, `SELECT quarantined_at,expired_at FROM junkpurge_quarantine
WHERE info_hash=$1 FOR UPDATE`, hash).Scan(&quarantinedAt, &expiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock quarantine verdict: %w", err)
	}
	if expiredAt != nil {
		return nil
	}
	if days <= 0 {
		days = 30
	}
	expiresAt := quarantinedAt.Add(time.Duration(days) * 24 * time.Hour)
	var alreadyRecorded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM torrent_verdict_state
WHERE info_hash=$1 AND verdict=$2 AND expires_at=$3)`, hash, verdicts.VerdictQuarantined, expiresAt).Scan(&alreadyRecorded); err != nil {
		return fmt.Errorf("check quarantine verdict: %w", err)
	}
	if alreadyRecorded {
		return nil
	}
	if err := verdicts.RecordTx(ctx, tx, verdicts.Event{
		InfoHash: hash, Verdict: verdicts.VerdictQuarantined,
		Mechanism: verdicts.MechanismJunkpurge, Reason: reason, ExpiresAt: &expiresAt,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
