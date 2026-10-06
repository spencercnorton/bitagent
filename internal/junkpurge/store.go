package junkpurge

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"go.uber.org/zap"
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

// RestoreQuarantined restores torrent, files and retained authentic sources,
// records local restore provenance with unknown counts, and queues a rematch
// atomically with removing the snapshot. Legacy snapshots without sources work
// too. The restore record uses the normal unknown-freshness window; it does not
// override authoritative tracker zero or liveness/ledger exclusions.
// 'extension' is a generated column on both tables, so it is excluded from the
// explicit column lists.
func RestoreQuarantined(ctx context.Context, pool *pgxpool.Pool, vstore *verdicts.Store, logger *zap.SugaredLogger, infoHashHex string) error {
	ih, err := protocol.ParseID(infoHashHex)
	if err != nil {
		return fmt.Errorf("invalid info_hash %q: %w", infoHashHex, err)
	}
	ihBytes := ih[:]
	job, err := processor.NewQueueJob(processor.MessageParams{
		InfoHashes:   []protocol.ID{ih},
		ClassifyMode: processor.ClassifyModeRematch,
	})
	if err != nil {
		return fmt.Errorf("build restore reprocess job: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var torrentSnap, filesSnap, sourcesSnap []byte
	if err := tx.QueryRow(ctx,
		`SELECT torrent_snapshot, files_snapshot, sources_snapshot FROM junkpurge_quarantine WHERE info_hash = $1 FOR UPDATE`,
		ihBytes).Scan(&torrentSnap, &filesSnap, &sourcesSnap); err != nil {
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
	if len(sourcesSnap) > 0 {
		// Restore registry entries if removed since quarantine, using only their
		// authentic captured metadata. Existing definitions/observations win.
		if _, err := tx.Exec(ctx, `
INSERT INTO torrent_sources (key, name, created_at, updated_at)
SELECT key, name, created_at, updated_at
FROM jsonb_populate_recordset(null::torrent_sources,
  (SELECT jsonb_agg(value->'source_metadata') FROM jsonb_array_elements($1::jsonb)))
ON CONFLICT (key) DO NOTHING`, sourcesSnap); err != nil {
			return fmt.Errorf("restore source registry: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO torrents_torrent_sources
  (source, info_hash, import_id, seeders, leechers, published_at, created_at, updated_at)
SELECT source, info_hash, import_id, seeders, leechers, published_at, created_at, updated_at
FROM jsonb_populate_recordset(null::torrents_torrent_sources, $1::jsonb)
WHERE info_hash = $2
ON CONFLICT (source, info_hash) DO NOTHING`, sourcesSnap, ihBytes); err != nil {
			return fmt.Errorf("restore torrent sources: %w", err)
		}
	}
	// A local action is useful freshness provenance, but carries no seed/leech
	// estimate, publish date, bloom data or claim that the swarm was observed.
	if _, err := tx.Exec(ctx, `
INSERT INTO torrent_sources (key, name, created_at, updated_at)
VALUES ($1, 'Local quarantine restore', now(), now())
ON CONFLICT (key) DO NOTHING`, model.SourceKeyQuarantineRestore); err != nil {
		return fmt.Errorf("register local quarantine restore: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO torrents_torrent_sources (source, info_hash, created_at, updated_at)
VALUES ($1, $2, now(), now())
ON CONFLICT (source, info_hash) DO UPDATE SET
  import_id = NULL, seeders = NULL, leechers = NULL,
  published_at = NULL, updated_at = now()`, model.SourceKeyQuarantineRestore, ihBytes); err != nil {
		return fmt.Errorf("record local quarantine restore: %w", err)
	}
	// Queue failure must retain the original snapshot and roll back all restored
	// rows. Notifications from the normal queue trigger publish only on commit.
	if _, err := tx.Exec(ctx, `
INSERT INTO queue_jobs
  (fingerprint, queue, status, payload, retries, max_retries, run_after, archival_duration, created_at, priority)
VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8::interval, clock_timestamp(), $9)
ON CONFLICT (fingerprint) WHERE status IN ('pending', 'retry') DO NOTHING`, job.Fingerprint, job.Queue, string(job.Status), job.Payload,
		job.Retries, job.MaxRetries, job.RunAfter, time.Duration(job.ArchivalDuration).String(), job.Priority); err != nil {
		return fmt.Errorf("enqueue restore reprocess: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM junkpurge_quarantine WHERE info_hash = $1`, ihBytes); err != nil {
		return err
	}
	if vstore != nil {
		if err := verdicts.RecordTx(ctx, tx, verdicts.Event{
			InfoHash: ihBytes, Verdict: verdicts.VerdictRestored,
			Mechanism: verdicts.MechanismOperator, Actor: "operator",
			Reason: "operator restored from junkpurge quarantine",
		}); err != nil {
			return fmt.Errorf("record quarantine restore: %w", err)
		}
	}
	return tx.Commit(ctx)
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
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Review actions are bound to an existing snapshot and serialize with a
	// concurrent restore. A missing entry is not authority to blacklist a hash.
	var present bool
	if err = tx.QueryRow(ctx, `SELECT true FROM junkpurge_quarantine WHERE info_hash=$1 FOR UPDATE`, ihBytes).Scan(&present); err != nil {
		return fmt.Errorf("quarantine entry not found: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO torrent_liveness (info_hash, status, last_observed_at, blacklisted_at)
VALUES ($1, 'dead', now(), now())
ON CONFLICT (info_hash) DO UPDATE SET status = 'dead', blacklisted_at = now(), updated_at = now()`, ihBytes); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM junkpurge_quarantine WHERE info_hash = $1`, ihBytes); err != nil {
		return err
	}
	if vstore != nil {
		if err := verdicts.RecordTx(ctx, tx, verdicts.Event{
			InfoHash: ihBytes, Verdict: verdicts.VerdictBlacklisted,
			Mechanism: verdicts.MechanismOperator, Actor: "operator",
			Reason: "operator confirmed delete from quarantine",
		}); err != nil {
			return fmt.Errorf("record quarantine deletion: %w", err)
		}
	}
	return tx.Commit(ctx)
}
