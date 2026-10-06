package junkpurge

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/verdicts"
)

// Expiry is maintenance before candidate processing, not an unbounded archive
// drain. Each cycle commits at most four finite chunks and leaves the remaining
// due rows eligible for later cycles. A chunk is atomic with its ledger writes.
const (
	quarantineExpiryChunkSize  = 256
	quarantineExpiryCycleLimit = 1024
)

const quarantineExpiryCandidatesSQL = `
SELECT info_hash FROM junkpurge_quarantine
WHERE quarantined_at < now() - make_interval(days => $1) AND expired_at IS NULL
ORDER BY info_hash
LIMIT $2
FOR UPDATE SKIP LOCKED`

func expireQuarantineChunk(ctx context.Context, pool *pgxpool.Pool, days, limit int, recordLedger bool) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin quarantine expiry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	n, err := expireQuarantineChunkTx(ctx, tx, days, limit, recordLedger)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit quarantine expiry: %w", err)
	}
	return n, nil
}

func expireQuarantineChunkTx(ctx context.Context, tx pgx.Tx, days, limit int, recordLedger bool) (int, error) {
	if limit <= 0 || limit > quarantineExpiryChunkSize {
		return 0, fmt.Errorf("invalid quarantine expiry chunk limit")
	}
	rows, err := tx.Query(ctx, quarantineExpiryCandidatesSQL, days, limit)
	if err != nil {
		return 0, fmt.Errorf("select quarantine expiry: %w", err)
	}
	var hashes [][]byte
	for rows.Next() {
		var hash []byte
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan quarantine expiry: %w", err)
		}
		hashes = append(hashes, hash)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("select quarantine expiry rows: %w", err)
	}
	if len(hashes) == 0 {
		return 0, nil
	}
	// The snapshot locks are held through marker, ledger and commit. Restore
	// and delete take the same snapshot-before-ledger order. Re-quarantine
	// cannot refresh an observed snapshot while this transition is in flight.
	if _, err := tx.Exec(ctx, quarantineExpireTombstoneSQL, hashes); err != nil {
		return 0, fmt.Errorf("mark quarantine expiry: %w", err)
	}
	if recordLedger {
		for _, hash := range hashes {
			if err := verdicts.RecordTx(ctx, tx, verdicts.Event{
				InfoHash: hash, Verdict: verdicts.VerdictTombstoned,
				Mechanism: verdicts.MechanismJunkpurge,
				Reason:    "quarantine review window lapsed; tombstoned, snapshot retained and restorable",
			}); err != nil {
				return 0, fmt.Errorf("record quarantine expiry: %w", err)
			}
		}
	}
	return len(hashes), nil
}
