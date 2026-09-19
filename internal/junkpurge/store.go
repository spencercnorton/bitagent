package junkpurge

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"go.uber.org/zap"
	"gorm.io/gorm/clause"
)

// QuarantineItem is one row in the review list.
type QuarantineItem struct {
	InfoHash      string  `json:"infoHash"` // hex
	TorrentName   string  `json:"torrentName"`
	Verdict       string  `json:"verdict"`
	Confidence    float64 `json:"confidence"`
	QuarantinedAt string  `json:"quarantinedAt"` // RFC3339 UTC
	DaysLeft      int     `json:"daysLeft"`
}

// ListQuarantine returns live (non-tombstoned) quarantined torrents newest-first,
// with days remaining in the review window (given quarantineDays), plus the total
// count of live rows.
//
// expired_at IS NULL is what makes the list mean "awaiting review". Tombstoned
// rows are retained forever and stay restorable by info_hash via
// RestoreQuarantined, but they are no longer actionable review work, and without
// this filter the list would fill with them permanently.
func ListQuarantine(ctx context.Context, pool *pgxpool.Pool, quarantineDays, limit, offset int) ([]QuarantineItem, int, error) {
	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM junkpurge_quarantine WHERE expired_at IS NULL`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := pool.Query(ctx, `
SELECT encode(info_hash, 'hex'), torrent_name, verdict, confidence, quarantined_at,
       GREATEST(0, $1 - floor(extract(epoch FROM (now() - quarantined_at)) / 86400)::int) AS days_left
FROM junkpurge_quarantine
WHERE expired_at IS NULL
ORDER BY quarantined_at DESC
LIMIT $2 OFFSET $3`, quarantineDays, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]QuarantineItem, 0, limit)
	for rows.Next() {
		var it QuarantineItem
		var qAt time.Time
		if err := rows.Scan(&it.InfoHash, &it.TorrentName, &it.Verdict, &it.Confidence, &qAt, &it.DaysLeft); err != nil {
			return nil, 0, err
		}
		it.QuarantinedAt = qAt.UTC().Format(time.RFC3339)
		out = append(out, it)
	}
	return out, total, rows.Err()
}

// RestoreQuarantined re-inserts the torrent + its file list from the snapshot,
// removes the quarantine row, and enqueues a rematch reprocess so torrent_contents
// (and the content match) regenerate — making the torrent searchable again.
// 'extension' is a generated column on both tables, so it is excluded from the
// explicit column lists.
func RestoreQuarantined(ctx context.Context, pool *pgxpool.Pool, daoQ *dao.Query, vstore *verdicts.Store, logger *zap.SugaredLogger, infoHashHex string) error {
	ih, err := protocol.ParseID(infoHashHex)
	if err != nil {
		return fmt.Errorf("invalid info_hash %q: %w", infoHashHex, err)
	}
	ihBytes := ih[:]

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var torrentSnap, filesSnap []byte
	if err := tx.QueryRow(ctx,
		`SELECT torrent_snapshot, files_snapshot FROM junkpurge_quarantine WHERE info_hash = $1`,
		ihBytes).Scan(&torrentSnap, &filesSnap); err != nil {
		return fmt.Errorf("quarantine entry not found: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO torrents (info_hash, name, size, private, created_at, updated_at, files_status, files_count)
SELECT info_hash, name, size, private, created_at, updated_at, files_status, files_count
FROM jsonb_populate_record(null::torrents, $1::jsonb)
ON CONFLICT (info_hash) DO NOTHING`, torrentSnap); err != nil {
		return fmt.Errorf("restore torrent row: %w", err)
	}
	if len(filesSnap) > 0 {
		if _, err := tx.Exec(ctx, `
INSERT INTO torrent_files (info_hash, "index", path, size, created_at, updated_at)
SELECT info_hash, "index", path, size, created_at, updated_at
FROM jsonb_populate_recordset(null::torrent_files, $1::jsonb)
ON CONFLICT DO NOTHING`, filesSnap); err != nil {
			return fmt.Errorf("restore torrent files: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM junkpurge_quarantine WHERE info_hash = $1`, ihBytes); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// T3: an operator restore is a gold label — record it. STRICTLY
	// best-effort (log-and-continue): the restore tx has committed and the
	// rematch enqueue below MUST run; returning an error here would leave
	// the torrent restored-but-unsearchable with no retry path (the
	// quarantine row is already gone) — the review-demonstrated P0.
	if vstore != nil {
		if verr := vstore.Record(ctx, verdicts.Event{
			InfoHash: ihBytes, Verdict: verdicts.VerdictRestored,
			Mechanism: verdicts.MechanismOperator, Actor: "operator",
			Reason: "operator restored from junkpurge quarantine",
		}); verr != nil && logger != nil {
			logger.Warnw("junkpurge restore: verdict record failed", "err", verr)
		}
	}

	// Re-queue classification (rematch) so torrent_contents + the content match
	// regenerate — without this the restored torrent has no content row and won't
	// appear in search/Torznab.
	job, err := processor.NewQueueJob(processor.MessageParams{
		InfoHashes:   []protocol.ID{ih},
		ClassifyMode: processor.ClassifyModeRematch,
	})
	if err != nil {
		return fmt.Errorf("restored, but building reprocess job failed: %w", err)
	}
	if err := daoQ.QueueJob.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&job); err != nil {
		return fmt.Errorf("restored, but enqueuing reprocess failed: %w", err)
	}
	return nil
}

// DeleteQuarantinedNow permanently removes a quarantine entry immediately
// (skipping the rest of the review window) and blacklists its info_hash so the
// crawler can't bring it back.
func DeleteQuarantinedNow(ctx context.Context, pool *pgxpool.Pool, vstore *verdicts.Store, logger *zap.SugaredLogger, infoHashHex string) error {
	ih, err := protocol.ParseID(infoHashHex)
	if err != nil {
		return fmt.Errorf("invalid info_hash %q: %w", infoHashHex, err)
	}
	ihBytes := ih[:]
	if _, err := pool.Exec(ctx, `
INSERT INTO torrent_liveness (info_hash, status, last_observed_at, blacklisted_at)
VALUES ($1, 'dead', now(), now())
ON CONFLICT (info_hash) DO UPDATE SET status = 'dead', blacklisted_at = now(), updated_at = now()`, ihBytes); err != nil {
		return err
	}
	if _, err = pool.Exec(ctx, `DELETE FROM junkpurge_quarantine WHERE info_hash = $1`, ihBytes); err != nil {
		return err
	}
	// Best-effort ledger record (log-and-continue): the delete has already
	// committed; surfacing a ledger error as a 500 would report failure for
	// an action that worked, and each retry appends a duplicate event.
	if vstore != nil {
		if verr := vstore.Record(ctx, verdicts.Event{
			InfoHash: ihBytes, Verdict: verdicts.VerdictBlacklisted,
			Mechanism: verdicts.MechanismOperator, Actor: "operator",
			Reason: "operator confirmed delete from quarantine",
		}); verr != nil && logger != nil {
			logger.Warnw("junkpurge delete-now: verdict record failed", "err", verr)
		}
	}
	return nil
}
