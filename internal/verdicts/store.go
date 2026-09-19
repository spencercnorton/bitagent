// Package verdicts is the persistence layer for the T3 verdict ledger
// (docs/design/verdict-ledger.md): an append-only event log plus a derived
// current-state row per infohash, written together in one transaction.
// Phase A shipped the write side (junkpurge/operator dual-write). Phase B
// adds the read side: BlockedSet, consulted by the torznab exclusion filter
// and the crawler's pre-fetch triage — shadow-metered until
// VERDICTS_READERS_ENABLED promotes them (see Config).
package verdicts

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
)

// Verdict values (design §2.2).
const (
	VerdictActive      = "active"
	VerdictSuspect     = "suspect"
	VerdictQuarantined = "quarantined"
	VerdictBlacklisted = "blacklisted"
	VerdictTombstoned  = "tombstoned"
	VerdictRestored    = "restored"
)

// Mechanism values (design §2.3) — the nine judging mechanisms. Phase A
// wired junkpurge + operator; phase C grows the set. The remaining
// mechanisms (crawler_drop, retention, liveness) land in later phase-C MRs.
const (
	MechanismJunkpurge        = "junkpurge"
	MechanismOperator         = "operator"
	MechanismClassifierDelete = "classifier_delete"
	MechanismContentFilter    = "content_filter"
	MechanismBlocking         = "blocking"
	MechanismCsam             = "csam"
)

// Event is one verdict observation. Evidence is mechanism-specific JSON and
// must never contain secrets or personal data.
type Event struct {
	InfoHash  []byte
	Verdict   string
	Mechanism string
	Reason    string
	Evidence  []byte // marshalled JSON or nil
	Actor     string // "" -> 'system'
	ExpiresAt *time.Time
}

type Store struct {
	pool lazy.Lazy[*pgxpool.Pool]
}

func NewStore(pool lazy.Lazy[*pgxpool.Pool]) *Store {
	return &Store{pool: pool}
}

// Record appends the event and upserts the derived state row in one
// transaction. Best-effort callers (phase-A dual-writers) treat an error as
// log-and-continue: the ledger must never break the mechanism it observes.
func (s *Store) Record(ctx context.Context, ev Event) error {
	if len(ev.InfoHash) == 0 || ev.Verdict == "" || ev.Mechanism == "" {
		return fmt.Errorf("verdicts: incomplete event")
	}
	actor := ev.Actor
	if actor == "" {
		actor = "system"
	}
	pool, err := s.pool.Get()
	if err != nil {
		return fmt.Errorf("verdicts: acquire pool: %w", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("verdicts: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// State FIRST: the upsert takes the per-hash row lock, serializing
	// concurrent Records so event ids per hash match state-write order —
	// the rebuild invariant phase B depends on (rebuild orders by id,
	// never created_at; proven divergent the other way round).
	if _, err := tx.Exec(ctx, `
INSERT INTO torrent_verdict_state (info_hash, verdict, mechanism, since, expires_at)
VALUES ($1, $2, $3, now(), $4)
ON CONFLICT (info_hash) DO UPDATE SET
  verdict    = excluded.verdict,
  mechanism  = excluded.mechanism,
  since      = now(),
  expires_at = excluded.expires_at,
  updated_at = now()`,
		ev.InfoHash, ev.Verdict, ev.Mechanism, ev.ExpiresAt); err != nil {
		return fmt.Errorf("verdicts: state upsert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO torrent_verdict_events (info_hash, verdict, mechanism, reason, evidence, actor, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		ev.InfoHash, ev.Verdict, ev.Mechanism, ev.Reason, ev.Evidence, actor, ev.ExpiresAt); err != nil {
		return fmt.Errorf("verdicts: event insert: %w", err)
	}
	return tx.Commit(ctx)
}

// blockingVerdicts are the states that exclude a hash from serving and from
// BEP-9 refetch (design §2.3). 'active', 'suspect' and 'restored' serve
// normally.
var blockingVerdicts = []string{VerdictQuarantined, VerdictBlacklisted, VerdictTombstoned}

// BlockedSet returns the subset of the supplied info_hashes whose CURRENT
// ledger verdict excludes them, keyed by the lowercase-hex rendering of the
// hash (the same keying the torznab adapter uses for liveness DeadSet). One
// indexed round trip on the state PK; readers treat an error as advisory
// (fail-open for serving/crawling — the ledger is a curator, not an
// availability dependency; the CSAM bloom egress gate is separate and
// unaffected).
func (s *Store) BlockedSet(ctx context.Context, infoHashes [][]byte) (map[string]struct{}, error) {
	if len(infoHashes) == 0 {
		return nil, nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, fmt.Errorf("verdicts: acquire pool: %w", err)
	}
	rows, err := pool.Query(ctx, `
SELECT info_hash FROM torrent_verdict_state
WHERE info_hash = ANY($1) AND verdict = ANY($2)`,
		infoHashes, blockingVerdicts)
	if err != nil {
		return nil, fmt.Errorf("verdicts: blocked set: %w", err)
	}
	defer rows.Close()
	blocked := map[string]struct{}{}
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("verdicts: blocked set scan: %w", err)
		}
		blocked[hex.EncodeToString(h)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("verdicts: blocked set rows: %w", err)
	}
	return blocked, nil
}
