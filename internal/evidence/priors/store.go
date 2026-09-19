package priors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
)

// Outcome enumerates the resolved-outcome column values. Pending
// rows have a null outcome; terminal rows are exactly one of these.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
)

// Prior is the in-memory shape of a grab_outcome_priors row.
type Prior struct {
	Key   FeatureKey
	Alpha int64
	Beta  int64
}

// Mean returns the posterior expected success rate. Beta(α, β) with
// the +1 priors baked into the schema means a fresh row already
// represents Beta(1, 1) — uniform on [0, 1] with mean 0.5.
func (p Prior) Mean() float64 {
	a := float64(p.Alpha)
	b := float64(p.Beta)
	return a / (a + b)
}

// Observations returns the total observed count behind the Beta.
// Used by the ranker to fall back to the global average when an
// individual key has too few observations to be trusted.
func (p Prior) Observations() int64 {
	return (p.Alpha - 1) + (p.Beta - 1)
}

// GrabAttempt is the in-memory shape of a torrent_grab_attempts row.
type GrabAttempt struct {
	ID             int64
	InfoHash       []byte
	Source         string
	SourceInstance string
	ReleaseTitle   string
	Features       []FeatureKey
	GrabbedAt      time.Time
	ResolvedAt     *time.Time
	Outcome        Outcome
}

// Store is the persistence boundary for the priors module.
type Store struct {
	pool lazy.Lazy[*pgxpool.Pool]
}

func NewStore(pool lazy.Lazy[*pgxpool.Pool]) *Store {
	return &Store{pool: pool}
}

// RecordGrab inserts a pending torrent_grab_attempts row. The
// features slice is JSON-encoded into the frozen feature column so
// the resolver can replay the same set on import without rederiving.
//
// Returns the new row id; callers may discard it (the resolution
// path looks up by info_hash, not id).
func (s *Store) RecordGrab(ctx context.Context, ev GrabAttempt) (int64, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("priors: acquire pool: %w", err)
	}
	featuresJSON, err := json.Marshal(ev.Features)
	if err != nil {
		return 0, fmt.Errorf("priors: marshal features: %w", err)
	}
	const q = `
insert into torrent_grab_attempts (
  info_hash, source, source_instance, release_title,
  features, grabbed_at
) values ($1, $2, $3, $4, $5, $6)
returning id`
	var id int64
	if err := pool.QueryRow(ctx, q,
		ev.InfoHash, ev.Source, ev.SourceInstance, ev.ReleaseTitle,
		featuresJSON, ev.GrabbedAt,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("priors: insert grab: %w", err)
	}
	return id, nil
}

// ResolvePendingByInfoHash finds every pending grab attempt for an
// infohash and marks it resolved with the given outcome. Returns the
// resolved attempts so the caller can apply prior updates with the
// frozen feature sets.
//
// Multiple pending rows are possible (rare; same hash grabbed by
// Sonarr and Radarr in error, or re-grabbed after a stall). The
// caller increments priors once per row — that's the correct
// observational unit.
//
// Prefer ResolvePendingAndIncrement on the import path so the
// pending→terminal transition and the priors α-bump land in a single
// transaction; this method exists for the rare callers that need
// just the resolution.
func (s *Store) ResolvePendingByInfoHash(
	ctx context.Context,
	infoHash []byte,
	outcome Outcome,
	resolvedAt time.Time,
) ([]GrabAttempt, error) {
	if len(infoHash) == 0 {
		return nil, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("priors: acquire pool: %w", err)
	}
	rows, err := pool.Query(ctx, resolveByHashQuery, infoHash, resolvedAt, string(outcome))
	if err != nil {
		return nil, fmt.Errorf("priors: resolve by hash: %w", err)
	}
	defer rows.Close()
	return scanAttempts(rows)
}

// resolveByHashQuery is shared between ResolvePendingByInfoHash and
// ResolvePendingAndIncrement so the SQL stays in one place.
const resolveByHashQuery = `
update torrent_grab_attempts
set resolved_at = $2,
    outcome     = $3
where resolved_at is null
  and info_hash = $1
returning id, info_hash, source, source_instance, release_title,
          features, grabbed_at, resolved_at, coalesce(outcome, '')`

// ResolvePendingAndIncrement atomically marks every pending grab
// attempt for an infohash terminal with the given outcome AND
// increments the matching α/β priors in one transaction. If the
// priors update fails, the resolution rolls back so the resolver can
// retry (rather than losing the success signal forever).
//
// Returns the resolved attempts (caller may use the slice for
// metrics) and the deduplicated feature keys that were applied.
func (s *Store) ResolvePendingAndIncrement(
	ctx context.Context,
	infoHash []byte,
	outcome Outcome,
	resolvedAt time.Time,
) ([]GrabAttempt, []FeatureKey, error) {
	if len(infoHash) == 0 {
		return nil, nil, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, nil, fmt.Errorf("priors: acquire pool: %w", err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("priors: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, resolveByHashQuery, infoHash, resolvedAt, string(outcome))
	if err != nil {
		return nil, nil, fmt.Errorf("priors: resolve by hash: %w", err)
	}
	resolved, err := scanAttempts(rows)
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if len(resolved) == 0 {
		// Nothing to do — commit a clean tx so callers can branch on
		// (resolved, nil, nil) without us leaving a rollback in flight.
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, fmt.Errorf("priors: commit tx: %w", err)
		}
		return nil, nil, nil
	}
	keys := mergeFeatures(resolved)
	if len(keys) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, fmt.Errorf("priors: commit tx: %w", err)
		}
		return resolved, nil, nil
	}
	var (
		successKeys []FeatureKey
		failureKeys []FeatureKey
	)
	switch outcome {
	case OutcomeSuccess:
		successKeys = keys
	case OutcomeFailure:
		failureKeys = keys
	default:
		return nil, nil, fmt.Errorf("priors: unsupported outcome %q", outcome)
	}
	if err := upsertCounts(ctx, tx, successKeys, "alpha"); err != nil {
		return nil, nil, err
	}
	if err := upsertCounts(ctx, tx, failureKeys, "beta"); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("priors: commit tx: %w", err)
	}
	return resolved, keys, nil
}

// DeletePendingByInfoHash removes any pending grab attempts for the
// given infohash without resolving them. Used by the private-tracker
// import path — see resolver.handlePrivateImport — so the expirer
// cannot later mis-attribute these rows as failures.
//
// Returns the number of rows deleted.
func (s *Store) DeletePendingByInfoHash(ctx context.Context, infoHash []byte) (int64, error) {
	if len(infoHash) == 0 {
		return 0, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("priors: acquire pool: %w", err)
	}
	tag, err := pool.Exec(ctx,
		`delete from torrent_grab_attempts
		 where info_hash = $1 and resolved_at is null`,
		infoHash,
	)
	if err != nil {
		return 0, fmt.Errorf("priors: delete pending by hash: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ExpirePending finds pending grab attempts older than cutoff and
// marks them resolved as failure. Returns the resolved attempts so
// the caller can apply β-side prior updates with the frozen feature
// sets.
//
// limit caps how many rows are processed per call; the worker passes
// in a configured batch size to keep the transaction short.
func (s *Store) ExpirePending(
	ctx context.Context,
	cutoff time.Time,
	resolvedAt time.Time,
	limit int,
) ([]GrabAttempt, error) {
	if limit <= 0 {
		return nil, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("priors: acquire pool: %w", err)
	}
	// Two safety properties matter here:
	//
	//  1. Re-check `resolved_at is null` in the outer UPDATE so an
	//     import resolver that won the race between the subquery and
	//     the UPDATE doesn't get its `success` clobbered with
	//     `failure`.
	//  2. Lock the candidate IDs `FOR UPDATE SKIP LOCKED` so two
	//     expirer workers in a multi-instance deployment cannot
	//     double-process the same rows.
	const q = `
update torrent_grab_attempts
set resolved_at = $1,
    outcome     = 'failure'
where id in (
  select id from torrent_grab_attempts
  where resolved_at is null
    and grabbed_at < $2
  order by grabbed_at asc
  limit $3
  for update skip locked
)
  and resolved_at is null
returning id, info_hash, source, source_instance, release_title,
          features, grabbed_at, resolved_at, coalesce(outcome, '')`
	rows, err := pool.Query(ctx, q, resolvedAt, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("priors: expire pending: %w", err)
	}
	defer rows.Close()
	return scanAttempts(rows)
}

// CountPending returns the number of unresolved grab attempts. Used
// by the metrics sampler to publish the pending-grab gauge so
// operators can spot the case where the resolver is under-counting
// imports.
func (s *Store) CountPending(ctx context.Context) (int64, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("priors: acquire pool: %w", err)
	}
	var n int64
	if err := pool.QueryRow(ctx,
		`select count(*) from torrent_grab_attempts where resolved_at is null`,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("priors: count pending: %w", err)
	}
	return n, nil
}

// PurgeResolved deletes resolved torrent_grab_attempts rows older
// than cutoff. Driven by a separate housekeeping worker; kept on the
// store so the SQL is colocated with the other lifecycle methods.
func (s *Store) PurgeResolved(ctx context.Context, cutoff time.Time) (int64, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("priors: acquire pool: %w", err)
	}
	tag, err := pool.Exec(ctx,
		`delete from torrent_grab_attempts
		 where resolved_at is not null and resolved_at < $1`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("priors: purge resolved: %w", err)
	}
	return tag.RowsAffected(), nil
}

// IncrementPriors applies α/β deltas to a batch of feature keys in a
// single transaction. successKeys and failureKeys may overlap (a
// single grab→import event observed against several features always
// updates α; an expired grab updates β); duplicates within either
// list are coalesced before the round trip.
//
// All updates are upserts: an unseen key starts at Beta(1, 1) and
// then has the appropriate side incremented.
func (s *Store) IncrementPriors(
	ctx context.Context,
	successKeys []FeatureKey,
	failureKeys []FeatureKey,
) error {
	if len(successKeys) == 0 && len(failureKeys) == 0 {
		return nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return fmt.Errorf("priors: acquire pool: %w", err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("priors: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := upsertCounts(ctx, tx, successKeys, "alpha"); err != nil {
		return err
	}
	if err := upsertCounts(ctx, tx, failureKeys, "beta"); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("priors: commit tx: %w", err)
	}
	return nil
}

func upsertCounts(ctx context.Context, tx pgx.Tx, keys []FeatureKey, column string) error {
	if len(keys) == 0 {
		return nil
	}
	keys = uniqueKeys(keys)
	// Whitelist: column comes from internal callers, but a defensive
	// switch is cheaper than the day someone adds a third side.
	switch column {
	case "alpha", "beta":
	default:
		return fmt.Errorf("priors: invalid column %q", column)
	}
	const queryFmt = `
insert into grab_outcome_priors (key_type, key_value, %[1]s, updated_at)
values ($1, $2, 2, now())
on conflict (key_type, key_value) do update set
  %[1]s      = grab_outcome_priors.%[1]s + 1,
  updated_at = now()`
	q := strings.NewReplacer("%[1]s", column).Replace(queryFmt)
	for _, k := range keys {
		if _, err := tx.Exec(ctx, q, k.Type, k.Value); err != nil {
			return fmt.Errorf("priors: upsert %s for %s/%s: %w", column, k.Type, k.Value, err)
		}
	}
	return nil
}

// LookupPriors returns the priors rows matching the requested keys.
// Keys not present in the table do not appear in the result; callers
// treat absence as Beta(1, 1) — the schema default.
//
// One round trip regardless of input slice length.
func (s *Store) LookupPriors(ctx context.Context, keys []FeatureKey) (map[FeatureKey]Prior, error) {
	out := make(map[FeatureKey]Prior, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	keys = uniqueKeys(keys)
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("priors: acquire pool: %w", err)
	}
	types := make([]string, 0, len(keys))
	values := make([]string, 0, len(keys))
	for _, k := range keys {
		types = append(types, k.Type)
		values = append(values, k.Value)
	}
	const q = `
select key_type, key_value, alpha, beta
from grab_outcome_priors
where (key_type, key_value) in (
  select * from unnest($1::text[], $2::text[])
)`
	rows, err := pool.Query(ctx, q, types, values)
	if err != nil {
		return nil, fmt.Errorf("priors: lookup: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Prior
		if err := rows.Scan(&p.Key.Type, &p.Key.Value, &p.Alpha, &p.Beta); err != nil {
			return nil, fmt.Errorf("priors: scan: %w", err)
		}
		out[p.Key] = p
	}
	return out, rows.Err()
}

// GlobalAverage returns the current global success ratio across all
// priors rows. Used by the ranker as the cold-start fallback when an
// individual feature has too few observations to be trusted.
//
// Returns (0.5, nil) when the table is empty — the schema default
// Beta(1, 1) mean. Errors are logged and the ranker falls back to
// 0.5; an unavailable global average is never a reason to fail
// search.
func (s *Store) GlobalAverage(ctx context.Context) (float64, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return 0.5, fmt.Errorf("priors: acquire pool: %w", err)
	}
	var sumAlpha, sumBeta int64
	if err := pool.QueryRow(ctx,
		`select coalesce(sum(alpha), 0), coalesce(sum(beta), 0) from grab_outcome_priors`,
	).Scan(&sumAlpha, &sumBeta); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0.5, nil
		}
		return 0.5, fmt.Errorf("priors: global average: %w", err)
	}
	if sumAlpha+sumBeta <= 0 {
		return 0.5, nil
	}
	return float64(sumAlpha) / float64(sumAlpha+sumBeta), nil
}

// uniqueKeys returns the input slice with duplicates removed. Order
// is not preserved — callers must not depend on it.
func uniqueKeys(keys []FeatureKey) []FeatureKey {
	if len(keys) <= 1 {
		return keys
	}
	seen := make(map[FeatureKey]struct{}, len(keys))
	out := keys[:0:len(keys)]
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

func scanAttempts(rows pgx.Rows) ([]GrabAttempt, error) {
	var out []GrabAttempt
	for rows.Next() {
		var (
			a            GrabAttempt
			featuresJSON []byte
			resolvedAt   *time.Time
			outcome      string
		)
		if err := rows.Scan(
			&a.ID, &a.InfoHash, &a.Source, &a.SourceInstance, &a.ReleaseTitle,
			&featuresJSON, &a.GrabbedAt, &resolvedAt, &outcome,
		); err != nil {
			return nil, fmt.Errorf("priors: scan: %w", err)
		}
		if len(featuresJSON) > 0 {
			if err := json.Unmarshal(featuresJSON, &a.Features); err != nil {
				return nil, fmt.Errorf("priors: decode features: %w", err)
			}
		}
		a.ResolvedAt = resolvedAt
		if outcome != "" {
			a.Outcome = Outcome(outcome)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
