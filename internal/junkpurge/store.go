package junkpurge

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/catalogueguard"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
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

// Evaluate bounds and project the corresponding payload in one MVCC statement.
// The locked re-read uses the same projection so a refreshed oversized snapshot
// cannot be materialized before its version/capacity refusal.
const boundedLegacySnapshotSQL = `SELECT
CASE WHEN b.bytes <= $2 AND b.rows <= $3 THEN q.torrent_snapshot END,
CASE WHEN b.bytes <= $2 AND b.rows <= $3 THEN q.files_snapshot END,
CASE WHEN b.bytes <= $2 AND b.rows <= $3 THEN q.sources_snapshot END,
q.quarantined_at,b.bytes,b.rows
FROM junkpurge_quarantine q CROSS JOIN LATERAL (
SELECT octet_length(q.torrent_snapshot::text)+coalesce(octet_length(q.files_snapshot::text),0)+coalesce(octet_length(q.sources_snapshot::text),0) AS bytes,
1+coalesce(jsonb_array_length(q.files_snapshot),0)+coalesce(jsonb_array_length(q.sources_snapshot),0) AS rows
) b WHERE q.info_hash=$1`

const legacySnapshotMaxBytes = 64 << 20
const legacySnapshotMaxRows = 65536

// RestoreQuarantined restores torrent, files and retained authentic sources,
// records local restore provenance with unknown counts, and queues a rematch
// atomically with removing the snapshot. Legacy snapshots without sources work
// too. The restore record uses the normal unknown-freshness window; it does not
// override authoritative tracker zero or liveness/ledger exclusions.
// A configured verdict ledger commits in that transaction too; ledger failure
// retains the snapshot and rolls back the restored rows and queued work.
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
	// Independent authority is not FK-bound to torrents. Freeze it before
	// reading the snapshot, then recheck the raw row and tags under its lock.
	if _, err := tx.Exec(ctx, `set local statement_timeout='15s'; set local lock_timeout='2s';
LOCK TABLE label_evidence,torrent_canonical_labels IN SHARE MODE;
LOCK TABLE torrent_verdict_state,torrent_verdict_events IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var snapshotBytes, snapshotRows int64
	var torrentSnap, filesSnap, sourcesSnap []byte
	var quarantinedAt time.Time
	if err := tx.QueryRow(ctx, boundedLegacySnapshotSQL, ihBytes, legacySnapshotMaxBytes, legacySnapshotMaxRows).
		Scan(&torrentSnap, &filesSnap, &sourcesSnap, &quarantinedAt, &snapshotBytes, &snapshotRows); err != nil {
		return fmt.Errorf("quarantine entry not found: %w", err)
	}
	if snapshotBytes > legacySnapshotMaxBytes || snapshotRows > legacySnapshotMaxRows {
		return cataloguerecovery.ErrCapacity
	}
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT (jsonb_populate_record(null::torrents,$2::jsonb)).info_hash=$1
AND NOT EXISTS(SELECT 1 FROM jsonb_populate_recordset(null::torrent_files,coalesce($3::jsonb,'[]'::jsonb)) WHERE info_hash IS DISTINCT FROM $1)
AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(coalesce($4::jsonb,'[]'::jsonb)) r WHERE (jsonb_populate_record(null::torrents_torrent_sources,r.value)).info_hash IS DISTINCT FROM $1)`, ihBytes, torrentSnap, filesSnap, sourcesSnap).Scan(&bound); err != nil {
		return err
	}
	if !bound {
		return cataloguerecovery.ErrConflict
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO torrents (info_hash, name, size, private, created_at, updated_at, files_status, files_count)
SELECT info_hash, name, size, private, created_at, updated_at, files_status, files_count
FROM jsonb_populate_record(null::torrents, $1::jsonb)
ON CONFLICT (info_hash) DO NOTHING`, torrentSnap); err != nil {
		return fmt.Errorf("restore torrent row: %w", err)
	}
	// Quarantine admission locks the raw torrent before its snapshot. Take the
	// same order, including when the crawler has already recreated the raw row.
	// The preliminary snapshot is only a proposal: lock and compare its complete
	// version before restoring any dependent rows or deleting that snapshot.
	var lockedHash []byte
	if err := tx.QueryRow(ctx, `SELECT info_hash FROM torrents WHERE info_hash=$1 FOR UPDATE`, ihBytes).Scan(&lockedHash); err != nil {
		return fmt.Errorf("lock restored torrent: %w", err)
	}
	var currentTorrent, currentFiles, currentSources []byte
	var currentQuarantinedAt time.Time
	if err := tx.QueryRow(ctx, boundedLegacySnapshotSQL+` FOR UPDATE OF q`, ihBytes, legacySnapshotMaxBytes, legacySnapshotMaxRows).
		Scan(&currentTorrent, &currentFiles, &currentSources, &currentQuarantinedAt, &snapshotBytes, &snapshotRows); err != nil {
		return fmt.Errorf("quarantine entry not found: %w", err)
	}
	if snapshotBytes > legacySnapshotMaxBytes || snapshotRows > legacySnapshotMaxRows {
		return cataloguerecovery.ErrCapacity
	}
	if !quarantinedAt.Equal(currentQuarantinedAt) || !bytes.Equal(torrentSnap, currentTorrent) ||
		!bytes.Equal(filesSnap, currentFiles) || !bytes.Equal(sourcesSnap, currentSources) {
		return fmt.Errorf("quarantine snapshot changed during restore; retry against its current version")
	}
	var sourceMatches, protected bool
	if err := tx.QueryRow(ctx, `SELECT
(t.name,t.size,t.private,t.created_at,t.updated_at,t.files_status,t.files_count) IS NOT DISTINCT FROM
(s.name,s.size,s.private,s.created_at,s.updated_at,s.files_status,s.files_count),
t.private OR EXISTS(SELECT 1 FROM torrent_canonical_labels WHERE info_hash=$1)
OR EXISTS(SELECT 1 FROM label_evidence WHERE info_hash=$1 AND source='qbittorrent' AND lower(btrim(category,$3)) IN('private','bitgrab'))
OR EXISTS(SELECT 1 FROM torrent_verdict_state WHERE info_hash=$1 AND (mechanism<>'junkpurge' OR verdict NOT IN('quarantined','tombstoned')))
FROM torrents t CROSS JOIN jsonb_populate_record(null::torrents,$2::jsonb) s WHERE t.info_hash=$1`, ihBytes, torrentSnap, catalogueguard.TagWhitespace).Scan(&sourceMatches, &protected); err != nil {
		return err
	}
	if protected {
		return cataloguerecovery.ErrProtected
	}
	if !sourceMatches {
		return cataloguerecovery.ErrConflict
	}
	var tagCount, tagBytes int64
	// Lock existing tags without returning their names first. The raw parent
	// lock excludes inserts; these SHARE locks exclude edits between the byte
	// measurement and the subsequent bounded name read.
	tagLocks, err := tx.Query(ctx, `SELECT 1 FROM torrent_tags WHERE info_hash=$1 LIMIT 65537 FOR SHARE`, ihBytes)
	if err != nil {
		return err
	}
	for tagLocks.Next() {
		tagCount++
	}
	err = tagLocks.Err()
	tagLocks.Close()
	if err != nil {
		return err
	}
	if tagCount > legacySnapshotMaxRows {
		return cataloguerecovery.ErrCapacity
	}
	if err := tx.QueryRow(ctx, `SELECT count(*),coalesce(sum(octet_length(name)),0) FROM (SELECT name FROM torrent_tags WHERE info_hash=$1 LIMIT 65537) bounded`, ihBytes).Scan(&tagCount, &tagBytes); err != nil {
		return err
	}
	if tagCount > legacySnapshotMaxRows || tagBytes > legacySnapshotMaxBytes {
		return cataloguerecovery.ErrCapacity
	}
	tags, err := tx.Query(ctx, `SELECT name FROM torrent_tags WHERE info_hash=$1 LIMIT 65537 FOR SHARE`, ihBytes)
	if err != nil {
		return err
	}
	for tags.Next() {
		var name string
		if err := tags.Scan(&name); err != nil {
			tags.Close()
			return err
		}
		if catalogueguard.ProtectedTag(name, true) {
			tags.Close()
			return cataloguerecovery.ErrProtected
		}
	}
	err = tags.Err()
	tags.Close()
	if err != nil {
		return err
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

// DeleteQuarantinedNow retains the legacy archive until a destructive retention
// policy and complete recovery caller contract have been qualified. An explicit
// operator request cannot substitute for missing complete source capture.
func DeleteQuarantinedNow(ctx context.Context, pool *pgxpool.Pool, vstore *verdicts.Store, logger *zap.SugaredLogger, infoHashHex string) error {
	if _, err := protocol.ParseID(infoHashHex); err != nil {
		return fmt.Errorf("invalid info_hash %q: %w", infoHashHex, err)
	}
	return cataloguerecovery.ErrDisabled
}
