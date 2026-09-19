package seeds

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
)

// Store is the persistence layer for the seeds worker. It owns the
// torrent_tracker_seeds ledger and mirrors positive results into the
// torrents_torrent_sources(source='tracker') surfacing row.
type Store struct {
	pool lazy.Lazy[*pgxpool.Pool]
}

func NewStore(pool lazy.Lazy[*pgxpool.Pool]) *Store {
	return &Store{pool: pool}
}

// SelectStale returns up to limit info-hashes that have never been scraped or
// whose last scrape is older than minAge, least-recently-checked first. The
// left join + "checked_at asc nulls first" ordering surfaces never-checked
// hashes ahead of stale ones.
func (s *Store) SelectStale(ctx context.Context, minAge time.Duration, limit int) ([][]byte, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("seeds: acquire pool: %w", err)
	}
	const q = `
select t.info_hash
from torrents t
left join torrent_tracker_seeds s on s.info_hash = t.info_hash
where s.info_hash is null
   or s.checked_at < now() - make_interval(secs => $1)
order by s.checked_at asc nulls first
limit $2`
	rows, err := pool.Query(ctx, q, minAge.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("seeds: select stale: %w", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("seeds: scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Every write is guarded by `where exists (select 1 from torrents ...)` so a
// torrent deleted by retention/junkpurge between select and persist yields a
// no-op instead of an FK violation that would fail the whole batch.
const ledgerUpsertSQL = `
insert into torrent_tracker_seeds
  (info_hash, tracker_known, seeders, leechers, completed, best_tracker, checked_at,
   peak_seeders, last_positive_at)
select $1, $2, $3::int, $4::int, $5::int, $6, now(),
   $3::int, case when $3::int > 0 then now() end
where exists (select 1 from torrents where info_hash = $1)
on conflict (info_hash) do update set
  tracker_known = excluded.tracker_known,
  -- history BEFORE overwriting: prev is the reading this scrape replaces;
  -- peak keeps the historical maximum; last_positive_at only advances on a
  -- live reading, preserving the "how long dead" clock through zeros and
  -- unknowns.
  prev_seeders     = torrent_tracker_seeds.seeders,
  peak_seeders     = case
                       when excluded.seeders is null then torrent_tracker_seeds.peak_seeders
                       else greatest(coalesce(torrent_tracker_seeds.peak_seeders, 0), excluded.seeders)
                     end,
  last_positive_at = case
                       when excluded.seeders > 0 then excluded.checked_at
                       else torrent_tracker_seeds.last_positive_at
                     end,
  seeders       = excluded.seeders,
  leechers      = excluded.leechers,
  completed     = excluded.completed,
  best_tracker  = excluded.best_tracker,
  checked_at    = excluded.checked_at`

const sourceUpsertSQL = `
insert into torrents_torrent_sources
  (source, info_hash, seeders, leechers, created_at, updated_at)
select 'tracker', $1, $2::int, $3::int, now(), now()
where exists (select 1 from torrents where info_hash = $1)
on conflict (source, info_hash) do update set
  seeders    = excluded.seeders,
  leechers   = excluded.leechers,
  updated_at = now()`

const sourceDeleteSQL = `
delete from torrents_torrent_sources where source = 'tracker' and info_hash = $1`

// denormSyncSQL recomputes the denormalized torrent_contents.seeders/leechers
// for a batch of hashes to exactly what the next classify/persist would set,
// mirroring model.Torrent.Seeders(): the authoritative 'tracker' source row
// wins outright when it carries a count (including 0 for a known-dead swarm);
// MAX across the remaining sources applies only when there is no tracker
// verdict. Per-column coalesce keeps the two columns independent, matching
// the Go read.
// The UPDATE is driven from the cycle's hash list (unnest) and LEFT JOINs the
// per-hash aggregate, so a hash whose only source row was just cleared (a
// tracker-unknown with no remaining source) has no aggregate row and is set
// to NULL rather than left carrying a stale positive count.
// Deliberately does not touch updated_at — that column carries classify-time
// staleness semantics — and tc.tsv is generated from search_string only, so
// this update never churns the FTS index.
const denormSyncSQL = `
update torrent_contents tc set
  seeders  = agg.seeders,
  leechers = agg.leechers
from (
  select h.info_hash, mx.seeders, mx.leechers
  from unnest($1::bytea[]) as h(info_hash)
  left join (
    select info_hash,
      coalesce(
        max(seeders)  filter (where source = 'tracker'),
        max(seeders)  filter (where source <> 'tracker')
      ) as seeders,
      coalesce(
        max(leechers) filter (where source = 'tracker'),
        max(leechers) filter (where source <> 'tracker')
      ) as leechers
    from torrents_torrent_sources
    where info_hash = any($1)
    group by info_hash
  ) mx on mx.info_hash = h.info_hash
) agg
where tc.info_hash = agg.info_hash
  and (tc.seeders is distinct from agg.seeders or tc.leechers is distinct from agg.leechers)`

// Persist writes one scrape cycle's results: the ledger row for every hash,
// plus a mirrored authoritative source row for every tracker-known hash —
// positive counts as before, and 0/0 for a known-zero verdict so downstream
// readers (the torznab authoritative-zero filter, Torrent.Seeders()) can tell
// a real dead swarm from a bloom-approximated zero. A tracker-unknown result
// clears any stale source row: honest-unknown leaves no 'tracker' row at all.
// Returns how many source rows were actually upserted and cleared. hashes
// fixes the iteration order so batch results map back deterministically.
func (s *Store) Persist(ctx context.Context, hashes [][]byte, outcomes map[string]*ScrapeOutcome) (upserted, cleared int, err error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, 0, fmt.Errorf("seeds: acquire pool: %w", err)
	}

	const (
		opUpsert = iota
		opDelete
	)
	batch := &pgx.Batch{}
	kinds := make([]int, 0, len(hashes))

	for _, h := range hashes {
		o := outcomes[string(h)]
		var seeders, leechers, completed *int32
		var best *string
		trackerKnown := false
		if o != nil {
			trackerKnown = o.TrackerKnown
			if o.TrackerKnown {
				sv, lv, cv := o.Seeders, o.Leechers, o.Completed
				seeders, leechers, completed = &sv, &lv, &cv
				if o.BestTracker != "" {
					bt := o.BestTracker
					best = &bt
				}
			}
		}
		batch.Queue(ledgerUpsertSQL, h, trackerKnown, seeders, leechers, completed, best)

		if o != nil && o.TrackerKnown {
			batch.Queue(sourceUpsertSQL, h, o.Seeders, o.Leechers)
			kinds = append(kinds, opUpsert)
		} else {
			batch.Queue(sourceDeleteSQL, h)
			kinds = append(kinds, opDelete)
		}
	}

	br := pool.SendBatch(ctx, batch)
	defer br.Close()

	for i := range hashes {
		if _, e := br.Exec(); e != nil { // ledger upsert
			return upserted, cleared, fmt.Errorf("seeds: ledger upsert: %w", e)
		}
		ct, e := br.Exec() // source op
		if e != nil {
			return upserted, cleared, fmt.Errorf("seeds: source op: %w", e)
		}
		if ct.RowsAffected() > 0 {
			if kinds[i] == opUpsert {
				upserted++
			} else {
				cleared++
			}
		}
	}
	return upserted, cleared, nil
}

// SyncDenormalizedCounts refreshes torrent_contents.seeders/leechers for the
// cycle's hashes from the just-written source rows. Without this the
// denormalized columns (what torznab ordering, the zero-seeder filter, and the
// UI read) stay frozen at classify time — measured at 62% drift against the
// ledger before this existed. Returns how many rows actually changed.
func (s *Store) SyncDenormalizedCounts(ctx context.Context, hashes [][]byte) (int64, error) {
	if len(hashes) == 0 {
		return 0, nil
	}

	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("seeds: acquire pool: %w", err)
	}

	ct, err := pool.Exec(ctx, denormSyncSQL, hashes)
	if err != nil {
		return 0, fmt.Errorf("seeds: denorm sync: %w", err)
	}

	return ct.RowsAffected(), nil
}

// CoverageCounts returns standing ledger coverage for the gauges: total rows
// checked, rows a tracker knows about, and rows currently carrying a positive
// (live) count.
func (s *Store) CoverageCounts(ctx context.Context) (ledger, trackerKnown, positive int64, err error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("seeds: acquire pool: %w", err)
	}
	const q = `
select
  count(*),
  count(*) filter (where tracker_known),
  count(*) filter (where seeders is not null and seeders > 0)
from torrent_tracker_seeds`
	err = pool.QueryRow(ctx, q).Scan(&ledger, &trackerKnown, &positive)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("seeds: coverage counts: %w", err)
	}
	return ledger, trackerKnown, positive, nil
}
