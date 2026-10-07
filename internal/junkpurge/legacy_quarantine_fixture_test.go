package junkpurge

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// This historical writer is test-only: legacy restore and expiry drills need
// to construct old partial snapshots. Runtime quarantine cannot call it.
func quarantineJunkTx(
	ctx context.Context,
	tx pgx.Tx,
	infoHashes [][]byte,
	minConfidence float64,
) ([][]byte, error) {
	if len(infoHashes) == 0 {
		return nil, nil
	}
	// ON CONFLICT DO UPDATE, not DO NOTHING: since expiry became a tombstone,
	// a quarantine row outlives the torrent's deletion, so a hash the crawler
	// re-acquires and the model re-judges as junk WILL collide with its own
	// tombstone. Under DO NOTHING that returns no rows, the DELETE FROM torrents
	// below is skipped, and the re-crawled junk torrent stays in search forever,
	// re-judged every RejudgeInterval and never removed. Re-quarantining resets
	// the review window and refreshes the snapshot against the row we are about
	// to delete — which is what keeps the restore path correct.
	//
	// A currently-live quarantine row cannot reach this branch: its torrent is
	// already gone from `torrents`, so the SELECT below produces nothing for it.
	rows, err := tx.Query(ctx, quarantineUpsertSQL, infoHashes, minConfidence)
	if err != nil {
		return nil, err
	}
	var inserted [][]byte
	for rows.Next() {
		var h []byte
		if scanErr := rows.Scan(&h); scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		inserted = append(inserted, h)
	}
	rows.Close()
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, rowsErr
	}
	if len(inserted) == 0 {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM torrents WHERE info_hash = ANY($1)`, inserted); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE junkpurge_judgments SET purged = true WHERE info_hash = ANY($1)`,
		inserted,
	); err != nil {
		return nil, err
	}
	return inserted, nil
}

const quarantineUpsertSQL = `
INSERT INTO junkpurge_quarantine (info_hash, torrent_name, verdict, confidence, torrent_snapshot, files_snapshot, sources_snapshot)
SELECT t.info_hash, t.name, j.verdict, j.confidence, to_jsonb(t),
       (SELECT jsonb_agg(to_jsonb(f)) FROM torrent_files f WHERE f.info_hash = t.info_hash),
       COALESCE((SELECT jsonb_agg(to_jsonb(ts) || jsonb_build_object('source_metadata', to_jsonb(s)))
        FROM torrents_torrent_sources ts JOIN torrent_sources s ON s.key = ts.source
        WHERE ts.info_hash = t.info_hash), '[]'::jsonb)
FROM torrents t
JOIN junkpurge_judgments j ON j.info_hash = t.info_hash
WHERE t.info_hash = ANY($1)
  AND t.private = false
  AND j.verdict = 'junk'
  AND j.confidence >= $2
  AND j.torrent_name = t.name
ON CONFLICT (info_hash) DO UPDATE SET
  torrent_name     = excluded.torrent_name,
  verdict          = excluded.verdict,
  confidence       = excluded.confidence,
  torrent_snapshot = excluded.torrent_snapshot,
  files_snapshot   = excluded.files_snapshot,
  sources_snapshot = excluded.sources_snapshot,
  quarantined_at   = now(),
  expired_at       = NULL
RETURNING info_hash`
