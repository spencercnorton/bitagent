package evidence

import (
	"context"
	"fmt"
	"time"
)

// bitagentIndexerPattern matches the *arr indexer name that fronts this
// instance's own Torznab endpoint (currently "BitAgent (Local DHT)").
// ILIKE-substring so a rename like "BitAgent (Prowlarr)" keeps counting.
const bitagentIndexerPattern = "%bitagent%"

// IndexerCount is one indexer's grab total within a stats window.
type IndexerCount struct {
	Indexer string
	Grabs   int
}

// IndexerStatsDay is one daily bucket of the grab trend.
type IndexerStatsDay struct {
	Date          time.Time
	TotalGrabs    int
	BitagentGrabs int
}

// IndexerStats aggregates *arr grab-webhook evidence by winning indexer.
// Counts only — release names never leave this query.
type IndexerStats struct {
	TotalGrabs    int
	BitagentGrabs int
	Indexers      []IndexerCount
	Days          []IndexerStatsDay
}

// indexerBreakdownQuery groups webhook_grab evidence by the indexer the
// *arr chose (raw_payload->release->indexer). Grabs whose payload lacks
// an indexer fold into '(unknown)'.
const indexerBreakdownQuery = `
select coalesce(nullif(raw_payload->'release'->>'indexer', ''), '(unknown)') as indexer,
       count(*)
from label_evidence
where source_kind = 'webhook_grab'
  and observed_at >= $1
group by 1
order by 2 desc, 1`

// indexerTrendQuery buckets the same rows by day, counting total grabs
// and the subset won by this instance's own indexer.
const indexerTrendQuery = `
select date_trunc('day', observed_at) as day,
       count(*),
       count(*) filter (where raw_payload->'release'->>'indexer' ilike $2)
from label_evidence
where source_kind = 'webhook_grab'
  and observed_at >= $1
group by 1
order by 1`

// IndexerStats returns grab-by-indexer aggregates since the given time.
func (s *Store) IndexerStats(ctx context.Context, since time.Time) (IndexerStats, error) {
	var out IndexerStats
	pool, err := s.pool.Get()
	if err != nil {
		return out, fmt.Errorf("evidence: acquire pool: %w", err)
	}

	rows, err := pool.Query(ctx, indexerBreakdownQuery, since)
	if err != nil {
		return out, fmt.Errorf("evidence: indexer breakdown: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ic IndexerCount
		if err := rows.Scan(&ic.Indexer, &ic.Grabs); err != nil {
			return out, fmt.Errorf("evidence: indexer scan: %w", err)
		}
		out.TotalGrabs += ic.Grabs
		out.Indexers = append(out.Indexers, ic)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("evidence: indexer rows: %w", err)
	}

	trend, err := pool.Query(ctx, indexerTrendQuery, since, bitagentIndexerPattern)
	if err != nil {
		return out, fmt.Errorf("evidence: indexer trend: %w", err)
	}
	defer trend.Close()
	for trend.Next() {
		var d IndexerStatsDay
		if err := trend.Scan(&d.Date, &d.TotalGrabs, &d.BitagentGrabs); err != nil {
			return out, fmt.Errorf("evidence: trend scan: %w", err)
		}
		out.BitagentGrabs += d.BitagentGrabs
		out.Days = append(out.Days, d)
	}
	if err := trend.Err(); err != nil {
		return out, fmt.Errorf("evidence: trend rows: %w", err)
	}
	return out, nil
}
