package liveness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
)

// Status enumerates the three values the torrent_liveness.status
// column may take. Defined here rather than in the migration so Go
// callers can match against typed constants.
type Status string

const (
	StatusAlive   Status = "alive"
	StatusSuspect Status = "suspect"
	StatusDead    Status = "dead"
)

// AliveSource is a free-form audit field that records which input
// flipped an infohash to alive. The set is intentionally open so
// future signal sources can extend it without a migration; current
// values are listed below.
const (
	AliveSourceQBState       = "qb_state"
	AliveSourceArrWebhook    = "arr_webhook_import"
	AliveSourceDHTRevalidate = "dht_revalidate"
	AliveSourceTrackerScrape = "tracker_scrape"
)

// Record is the in-memory shape of a torrent_liveness row. Only
// fields the resolver and revalidator actually read are projected.
type Record struct {
	InfoHash            []byte
	Status              Status
	LastQBState         string
	SuspectFirstSeenAt  *time.Time
	SuspectObservations int
	LastObservedAt      time.Time
	BlacklistedAt       *time.Time
	NextRevalidateAt    *time.Time
	AliveSource         string
	UpdatedAt           time.Time
}

// Store is the persistence boundary for torrent_liveness. It is
// deliberately small: callers issue typed transitions, the store
// translates them to SQL. There is no general-purpose Update — each
// transition has its own method so the SQL stays explicit and the
// state machine in resolver.go can be read without chasing helpers.
type Store struct {
	pool lazy.Lazy[*pgxpool.Pool]
}

// NewStore returns a Store bound to the given lazy pool.
func NewStore(pool lazy.Lazy[*pgxpool.Pool]) *Store {
	return &Store{pool: pool}
}

// Get returns the current liveness record for an infohash, or
// (nil, nil) if no row exists.
func (s *Store) Get(ctx context.Context, infoHash []byte) (*Record, error) {
	if len(infoHash) == 0 {
		return nil, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `
select info_hash, status, last_qb_state, suspect_first_seen_at,
       suspect_observations, last_observed_at, blacklisted_at,
       next_revalidate_at, alive_source, updated_at
from torrent_liveness
where info_hash = $1`
	row := pool.QueryRow(ctx, q, infoHash)
	var (
		rec        Record
		state      *string
		firstSeen  *time.Time
		blacklist  *time.Time
		revalidate *time.Time
		source     *string
	)
	if err := row.Scan(
		&rec.InfoHash, &rec.Status, &state, &firstSeen,
		&rec.SuspectObservations, &rec.LastObservedAt, &blacklist,
		&revalidate, &source, &rec.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("liveness: get: %w", err)
	}
	if state != nil {
		rec.LastQBState = *state
	}
	rec.SuspectFirstSeenAt = firstSeen
	rec.BlacklistedAt = blacklist
	rec.NextRevalidateAt = revalidate
	if source != nil {
		rec.AliveSource = *source
	}
	return &rec, nil
}

// MarkAlive upserts an infohash as alive. Any prior suspect/dead
// state is cleared. observedAt is the source's reported timestamp;
// updated_at uses now() at the database for consistency.
func (s *Store) MarkAlive(ctx context.Context, infoHash []byte, observedAt time.Time, qbState, source string) error {
	pool, err := s.pool.Get()
	if err != nil {
		return fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `
insert into torrent_liveness (
  info_hash, status, last_qb_state, suspect_first_seen_at,
  suspect_observations, last_observed_at, blacklisted_at,
  next_revalidate_at, alive_source, updated_at
) values ($1, 'alive', $2, null, 0, $3, null, null, $4, now())
on conflict (info_hash) do update set
  status                = 'alive',
  last_qb_state         = excluded.last_qb_state,
  suspect_first_seen_at = null,
  suspect_observations  = 0,
  last_observed_at      = excluded.last_observed_at,
  blacklisted_at        = null,
  next_revalidate_at    = null,
  alive_source          = excluded.alive_source,
  updated_at            = now()`
	_, err = pool.Exec(ctx, q, infoHash, nullableString(qbState), observedAt, nullableString(source))
	if err != nil {
		return fmt.Errorf("liveness: mark alive: %w", err)
	}
	return nil
}

// RecordSuspect inserts or updates the row for a single suspect
// observation. State machine on conflict:
//
//   - no row    → insert with status='suspect', count=1.
//   - alive row → flip to status='suspect', start the
//     suspect_first_seen_at clock, count=1. We treat this as the
//     first suspect observation in a new stall window — accumulating
//     against a count that started life under "alive" would break
//     the threshold semantics.
//   - suspect   → keep status, increment count, preserve
//     suspect_first_seen_at, bump last_observed_at. EXCEPTION: a suspect
//     row qB has never observed (last_qb_state IS NULL — charged only by
//     remote scrape zeros via RecordSuspectBatch) restarts the window
//     (count=1, fresh first_seen): remote evidence must not pre-charge the
//     local promotion gates.
//   - dead      → no change. Promotion to dead is sticky; only
//     MarkAliveFromDHT or an alive observation can revive it.
//
// Returns the post-update record so the resolver can decide whether
// the threshold has been crossed.
func (s *Store) RecordSuspect(ctx context.Context, infoHash []byte, observedAt time.Time, qbState string) (*Record, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `
insert into torrent_liveness (
  info_hash, status, last_qb_state, suspect_first_seen_at,
  suspect_observations, last_observed_at, updated_at
) values ($1, 'suspect', $2, $3, 1, $3, now())
on conflict (info_hash) do update set
  status                = case
                            when torrent_liveness.status = 'dead' then 'dead'
                            else 'suspect'
                          end,
  last_qb_state         = excluded.last_qb_state,
  suspect_first_seen_at = case
                            when torrent_liveness.status = 'alive'
                              or torrent_liveness.last_qb_state is null then excluded.suspect_first_seen_at
                            else coalesce(torrent_liveness.suspect_first_seen_at, excluded.suspect_first_seen_at)
                          end,
  suspect_observations  = case
                            when torrent_liveness.status = 'alive'
                              or torrent_liveness.last_qb_state is null then 1
                            else torrent_liveness.suspect_observations + 1
                          end,
  last_observed_at      = excluded.last_observed_at,
  updated_at            = now()
returning info_hash, status, coalesce(last_qb_state, ''), suspect_first_seen_at,
          suspect_observations, last_observed_at, blacklisted_at,
          next_revalidate_at, coalesce(alive_source, ''), updated_at`
	var (
		rec        Record
		firstSeen  *time.Time
		blacklist  *time.Time
		revalidate *time.Time
	)
	if err := pool.QueryRow(ctx, q, infoHash, nullableString(qbState), observedAt).Scan(
		&rec.InfoHash, &rec.Status, &rec.LastQBState, &firstSeen,
		&rec.SuspectObservations, &rec.LastObservedAt, &blacklist,
		&revalidate, &rec.AliveSource, &rec.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("liveness: record suspect: %w", err)
	}
	rec.SuspectFirstSeenAt = firstSeen
	rec.BlacklistedAt = blacklist
	rec.NextRevalidateAt = revalidate
	return &rec, nil
}

// MarkDead transitions an existing row to dead and arms its
// re-validation timer. Caller is the resolver, not external code —
// the threshold check lives in resolver.go.
//
// The explicit “::timestamptz“ cast on $2's first use resolves a
// PostgreSQL prepared-statement type-inference ambiguity that surfaced
// after the priors-learner merge (kleos-v1.18.0 → cf7babd8). Without
// it, the planner sees $2 in both “blacklisted_at = $2“ and “$2 +
// $3::interval“ and can't pick a single type — the “+“ operator has
// multiple overloads (timestamptz+interval, text+text via concat, etc.)
// and the column-side context isn't strong enough to anchor the
// inference. The driver returned “ERROR: inconsistent types deduced
// for parameter $2 (SQLSTATE 42P08)“ on every call. Pinning $2 to
// “timestamptz“ once is enough; the second use inherits the type.
//
// `blacklisted_at` and `next_revalidate_at` are both `timestamptz`
// per migrations/00025_torrent_liveness.sql.
func (s *Store) MarkDead(ctx context.Context, infoHash []byte, blacklistedAt time.Time, ttl time.Duration) error {
	pool, err := s.pool.Get()
	if err != nil {
		return fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `
update torrent_liveness
set status             = 'dead',
    blacklisted_at     = $2::timestamptz,
    next_revalidate_at = $2::timestamptz + $3::interval,
    updated_at         = now()
where info_hash = $1`
	_, err = pool.Exec(ctx, q, infoHash, blacklistedAt, ttl.String())
	if err != nil {
		return fmt.Errorf("liveness: mark dead: %w", err)
	}
	return nil
}

// RearmRevalidate pushes next_revalidate_at forward without
// changing the status. Used by the revalidator when a DHT lookup
// against a dead hash still returns no peers.
func (s *Store) RearmRevalidate(ctx context.Context, infoHash []byte, ttl time.Duration) error {
	pool, err := s.pool.Get()
	if err != nil {
		return fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `
update torrent_liveness
set next_revalidate_at = now() + $2::interval,
    updated_at         = now()
where info_hash = $1 and status = 'dead'`
	_, err = pool.Exec(ctx, q, infoHash, ttl.String())
	if err != nil {
		return fmt.Errorf("liveness: rearm revalidate: %w", err)
	}
	return nil
}

// DueForRevalidation returns up to limit dead infohashes whose
// next_revalidate_at has elapsed.
func (s *Store) DueForRevalidation(ctx context.Context, limit int) ([][]byte, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `
select info_hash
from torrent_liveness
where status = 'dead'
  and next_revalidate_at is not null
  and next_revalidate_at <= now()
order by next_revalidate_at asc
limit $1`
	rows, err := pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("liveness: due-for-revalidation: %w", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("liveness: scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// IsDead reports whether the given infohash is currently dead.
// Returns (false, nil) for an absent row — only an explicit dead
// status filters a Torznab response.
func (s *Store) IsDead(ctx context.Context, infoHash []byte) (bool, error) {
	if len(infoHash) == 0 {
		return false, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return false, fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `select 1 from torrent_liveness where info_hash = $1 and status = 'dead'`
	var one int
	if err := pool.QueryRow(ctx, q, infoHash).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("liveness: is-dead: %w", err)
	}
	return true, nil
}

// DeadSet returns the subset of the supplied infohashes that are
// currently in status='dead'. The result is a set keyed by the hex
// rendering of the hash for cheap membership tests in the caller.
// One round trip regardless of slice length.
func (s *Store) DeadSet(ctx context.Context, infoHashes [][]byte) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if len(infoHashes) == 0 {
		return out, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `
select info_hash
from torrent_liveness
where status = 'dead' and info_hash = any($1::bytea[])`
	rows, err := pool.Query(ctx, q, infoHashes)
	if err != nil {
		return nil, fmt.Errorf("liveness: dead-set: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("liveness: scan: %w", err)
		}
		out[hashKey(h)] = struct{}{}
	}
	return out, rows.Err()
}

// Count returns the row count for a given status. Used by the
// metrics sampler to publish the blacklist-size gauge.
func (s *Store) Count(ctx context.Context, status Status) (int64, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("liveness: acquire pool: %w", err)
	}
	var n int64
	if err := pool.QueryRow(ctx,
		`select count(*) from torrent_liveness where status = $1`, status,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("liveness: count: %w", err)
	}
	return n, nil
}

// HashKey returns the canonical map key for an info_hash byte slice.
// Exported as a package-level helper so callers building DeadSet
// query inputs can produce matching keys without re-encoding.
func HashKey(infoHash []byte) string {
	return hashKey(infoHash)
}

func hashKey(infoHash []byte) string {
	// Hex is the natural human-readable form of an info_hash and
	// matches every other place in this codebase. Keeping the
	// helper internal lets us swap encodings later without churn.
	const hexAlphabet = "0123456789abcdef"
	out := make([]byte, len(infoHash)*2)
	for i, b := range infoHash {
		out[2*i] = hexAlphabet[b>>4]
		out[2*i+1] = hexAlphabet[b&0x0f]
	}
	return string(out)
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// MarkAliveBatch flips the given hashes to alive with the given source
// attribution — delta-only: it UPDATEs rows that are currently suspect or
// dead and deliberately does not INSERT. The seeds cycle calls this with up
// to BatchSize positive hashes every interval; creating alive rows for every
// healthy swarm would bloat torrent_liveness with no signal (absence of a row
// already means "no negative evidence"), and re-upserting already-alive rows
// would rewrite the whole alive population daily. A positive tracker scrape
// IS a legitimate revival for a dead row: real seeders exist, so the swarm is
// not junk-dead. Junkpurge's PERMANENT blacklist rows are the exception and
// are excluded: they are distinguishable as status='dead' with
// next_revalidate_at IS NULL (the resolver's MarkDead always arms the timer;
// junkpurge deliberately does not, keeping its rows outside the revalidator),
// and clearing them would resurrect purged junk into Torznab after a
// re-crawl + one positive scrape — the exact failure class the quarantine
// ladder exists to prevent.
// Returns how many rows actually transitioned.
func (s *Store) MarkAliveBatch(ctx context.Context, infoHashes [][]byte, observedAt time.Time, source string) (int64, error) {
	if len(infoHashes) == 0 {
		return 0, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `
update torrent_liveness set
  status                = 'alive',
  suspect_first_seen_at = null,
  suspect_observations  = 0,
  last_observed_at      = $2,
  blacklisted_at        = null,
  next_revalidate_at    = null,
  alive_source          = $3,
  updated_at            = now()
where info_hash = any($1)
  and status <> 'alive'
  and not (status = 'dead' and next_revalidate_at is null)`
	ct, err := pool.Exec(ctx, q, infoHashes, observedAt, source)
	if err != nil {
		return 0, fmt.Errorf("liveness: mark alive batch: %w", err)
	}
	return ct.RowsAffected(), nil
}

// RecordSuspectBatch records one remote suspect observation per hash — but
// ONLY on rows qBittorrent has never observed (last_qb_state IS NULL). This
// keeps the promotion arithmetic pure: the resolver's suspect→dead gates
// (MinObservations over StallThreshold) read suspect_observations and
// suspect_first_seen_at, and letting remote scrape zeros pre-charge those
// counters would let a single transient qB observation insta-promote a fresh
// grab to a 30-day blacklist. Scrape-only rows accumulate their own
// observations (evidence for the future verdict ledger, inert for promotion);
// the moment qB evidence arrives, RecordSuspect restarts the window (see its
// last_qb_state clause). Rows alive on LOCAL evidence (qb_state /
// arr_webhook_import) are likewise skipped — a remote zero must not open a
// stall window on a torrent qBittorrent is demonstrably seeding. Duplicate
// hashes are deduped (ON CONFLICT cannot touch a row twice per statement).
// Returns how many rows were inserted or updated.
func (s *Store) RecordSuspectBatch(ctx context.Context, infoHashes [][]byte, observedAt time.Time) (int64, error) {
	if len(infoHashes) == 0 {
		return 0, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("liveness: acquire pool: %w", err)
	}
	const q = `
insert into torrent_liveness (
  info_hash, status, suspect_first_seen_at,
  suspect_observations, last_observed_at, updated_at
)
select distinct u.h, 'suspect', $2::timestamptz, 1, $2, now()
from unnest($1::bytea[]) as u(h)
on conflict (info_hash) do update set
  status                = case
                            when torrent_liveness.status = 'dead' then 'dead'
                            else 'suspect'
                          end,
  suspect_first_seen_at = case
                            when torrent_liveness.status = 'alive' then excluded.suspect_first_seen_at
                            else coalesce(torrent_liveness.suspect_first_seen_at, excluded.suspect_first_seen_at)
                          end,
  suspect_observations  = case
                            when torrent_liveness.status = 'alive' then 1
                            else torrent_liveness.suspect_observations + 1
                          end,
  last_observed_at      = excluded.last_observed_at,
  updated_at            = now()
where torrent_liveness.last_qb_state is null
  and not (torrent_liveness.status = 'alive'
	and coalesce(torrent_liveness.alive_source, '') in ('qb_state', 'arr_webhook_import'))`
	ct, err := pool.Exec(ctx, q, infoHashes, observedAt)
	if err != nil {
		return 0, fmt.Errorf("liveness: record suspect batch: %w", err)
	}
	return ct.RowsAffected(), nil
}
