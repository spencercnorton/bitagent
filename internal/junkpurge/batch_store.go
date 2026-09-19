package junkpurge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	runStateBuilding       = "building"
	runStateActive         = "active"
	runStateFinalizing     = "finalizing"
	runStateCompleted      = "completed"
	runStateDryRun         = "dry_run"
	runStateBreakerBlocked = "breaker_blocked"
	runStateFailed         = "failed"

	itemStatePending        = "pending"
	itemStateSubmitted      = "submitted"
	itemStateSucceeded      = "succeeded"
	itemStateRetryableError = "retryable_error"
	itemStateTerminalError  = "terminal_error"
	itemStateApplied        = "applied"
	itemStateAbandoned      = "abandoned"

	batchAdmissionLockSQL = `
SELECT pg_advisory_xact_lock(
  hashtext('bitagent'), hashtext('junkpurge_batch')
)`

	// batchAttemptEligibilityQuery is the last local egress gate before a
	// torrent name is serialized into provider JSONL. The explicit qBittorrent
	// private/bitgrab clause is intentionally retained even though the broader
	// no-evidence condition currently subsumes it: privacy must remain pinned
	// if ordinary evidence eligibility is relaxed later.
	batchAttemptEligibilityQuery = `
SELECT i.custom_id
FROM junkpurge_batch_items i
JOIN torrents t ON t.info_hash=i.info_hash
WHERE i.run_id=$1
  AND i.state IN ('pending','retryable_error')
  AND t.private = false
  AND t.name=i.torrent_name
  AND EXISTS (
    SELECT 1 FROM torrent_contents tc
    WHERE tc.info_hash=t.info_hash
      AND tc.content_type IN ('movie','tv_show')
      AND tc.content_id IS NULL
      AND tc.created_at < now()-make_interval(secs => $2)
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_contents m
    WHERE m.info_hash=t.info_hash AND m.content_id IS NOT NULL
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_canonical_labels l WHERE l.info_hash=t.info_hash
  )
  AND NOT EXISTS (
    SELECT 1 FROM label_evidence private_evidence
    WHERE private_evidence.info_hash=t.info_hash
      AND private_evidence.source='qbittorrent'
      AND lower(private_evidence.category) IN ('private','bitgrab')
  )
  AND NOT EXISTS (
    SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash
  )
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_judgments j
    WHERE j.info_hash=t.info_hash AND j.judged_at >= $3
  )
ORDER BY i.ordinal`
)

type batchRun struct {
	ID               int64
	State            string
	Model            string
	ProviderBaseURL  string
	Endpoint         string
	PromptVersion    string
	SystemPrompt     string
	MaxTokens        int
	ReasoningEffort  string
	CompletionWindow string
	MinAge           time.Duration
	MinConfidence    float64
	MaxJunkRate      float64
	EnablePurge      bool
	QuarantineDays   int
	FailureCooldown  time.Duration
	MaxAttempts      int
	AmbiguityGrace   time.Duration
	ItemCount        int
	CreatedAt        time.Time
}

type batchItem struct {
	RunID        int64
	Ordinal      int
	CustomID     string
	InfoHash     []byte
	TorrentName  string
	State        string
	AttemptCount int
}

type batchAttempt struct {
	ID                    int64
	RunID                 int64
	AttemptNo             int
	State                 string
	InputFilename         string
	InputSHA256           string
	InputBytes            int64
	ItemCount             int
	InputFileID           string
	ProviderBatchID       string
	OutputFileID          string
	ErrorFileID           string
	ProviderStatus        string
	RequestTotal          int
	RequestCompleted      int
	RequestFailed         int
	SubmissionAttemptedAt *time.Time
	SubmittedAt           *time.Time
	TerminalAt            *time.Time
	IngestedAt            *time.Time
	LastError             string
	Payload               []byte
	Endpoint              string
	CompletionWindow      string
	MaxAttempts           int
	AmbiguityGrace        time.Duration
	ProviderBaseURL       string
	Model                 string
}

type batchItemResult struct {
	CustomID        string
	Succeeded       bool
	Retryable       bool
	Judgment        Judgment
	ProviderRequest string
	ResponseStatus  int
	ErrorCode       string
	ErrorMessage    string
	Usage           tokenUsage
}

type batchIngestSummary struct {
	RunID             int64
	AttemptID         int64
	AttemptNo         int
	Succeeded         int
	RetryableFailures int
	TerminalFailures  int
	Missing           int
	RunState          string
	Usage             tokenUsage
}

type batchFinalizeSummary struct {
	RunID             int64
	State             string
	Judged            int
	Junk              int
	WouldDelete       int
	QuarantinedHashes [][]byte
	QuarantineDays    int
	Usage             tokenUsage
}

type batchPayloadBuilder func(
	run batchRun,
	attemptNo int,
	items []batchItem,
) (payload []byte, filename, sha256 string, err error)

func createBatchRun(ctx context.Context, pool *pgxpool.Pool, cfg Config) (*batchRun, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("junkpurge batch: begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize the capacity check across replicas. The row-level candidate
	// locks below prevent duplicate items; this advisory lock additionally
	// makes LLMBatchMaxInFlight a hard ceiling instead of a best-effort one.
	if _, err := tx.Exec(ctx, batchAdmissionLockSQL); err != nil {
		return nil, fmt.Errorf("junkpurge batch: acquire reservation lock: %w", err)
	}
	var active int
	if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM junkpurge_batch_runs
WHERE state IN ('building','active','finalizing')`).Scan(&active); err != nil {
		return nil, fmt.Errorf("junkpurge batch: count active runs: %w", err)
	}
	if active >= cfg.LLMBatchMaxInFlight {
		return nil, nil
	}

	candidates, err := findCandidatesQuery(ctx, tx, cfg)
	if err != nil {
		return nil, fmt.Errorf("junkpurge batch: select candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	quarantineDays := cfg.QuarantineDays
	if quarantineDays <= 0 {
		quarantineDays = 30
	}
	run := batchRun{
		State:            runStateBuilding,
		Model:            cfg.LLMModel,
		ProviderBaseURL:  strings.TrimRight(cfg.LLMBaseURL, "/"),
		Endpoint:         "/v1/chat/completions",
		PromptVersion:    judgePromptVersion,
		SystemPrompt:     judgeInstructions + "\n/no_think",
		MaxTokens:        judgeMaxTokens,
		ReasoningEffort:  judgeReasoning,
		CompletionWindow: cfg.LLMBatchCompletionWindow,
		MinAge:           cfg.MinAge,
		MinConfidence:    cfg.MinConfidence,
		MaxJunkRate:      cfg.MaxJunkRate,
		EnablePurge:      cfg.EnablePurge,
		QuarantineDays:   quarantineDays,
		FailureCooldown:  cfg.LLMBatchFailureCooldown,
		MaxAttempts:      cfg.LLMBatchMaxAttempts,
		AmbiguityGrace:   cfg.LLMBatchAmbiguityGrace,
	}
	err = tx.QueryRow(ctx, `
INSERT INTO junkpurge_batch_runs (
  state, model, provider_base_url, endpoint, prompt_version, system_prompt,
  max_completion_tokens, reasoning_effort, completion_window,
  min_age_seconds, min_confidence, max_junk_rate, enable_purge,
  quarantine_days, failure_cooldown_seconds, max_attempts,
  ambiguity_grace_seconds
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
RETURNING id, created_at`,
		run.State, run.Model, run.ProviderBaseURL, run.Endpoint, run.PromptVersion,
		run.SystemPrompt, run.MaxTokens, run.ReasoningEffort,
		run.CompletionWindow, int64(run.MinAge/time.Second),
		run.MinConfidence, run.MaxJunkRate, run.EnablePurge,
		run.QuarantineDays, int64(run.FailureCooldown/time.Second),
		run.MaxAttempts, int64(run.AmbiguityGrace/time.Second),
	).Scan(&run.ID, &run.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("junkpurge batch: insert run: %w", err)
	}

	for index, c := range candidates {
		ordinal := index + 1
		customID := fmt.Sprintf("jp:%d:%06d", run.ID, ordinal)
		if _, err := tx.Exec(ctx, `
INSERT INTO junkpurge_batch_items (
  run_id, ordinal, custom_id, info_hash, torrent_name, state
) VALUES ($1,$2,$3,$4,$5,$6)`,
			run.ID, ordinal, customID, c.infoHash, c.name, itemStatePending,
		); err != nil {
			return nil, fmt.Errorf("junkpurge batch: reserve item %s: %w",
				hex.EncodeToString(c.infoHash), err)
		}
	}
	run.ItemCount = len(candidates)
	if _, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_runs
SET state=$2, item_count=$3, updated_at=now()
WHERE id=$1`,
		run.ID, runStateActive, run.ItemCount,
	); err != nil {
		return nil, fmt.Errorf("junkpurge batch: activate run: %w", err)
	}
	run.State = runStateActive
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("junkpurge batch: commit reserve: %w", err)
	}
	return &run, nil
}

type candidateQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func findCandidatesQuery(ctx context.Context, q candidateQuerier, cfg Config) ([]candidate, error) {
	return findCandidatesWithQuery(ctx, q, cfg, candidateQuery)
}

func findCandidatesWithQuery(
	ctx context.Context,
	q candidateQuerier,
	cfg Config,
	query string,
) ([]candidate, error) {
	return queryCandidates(
		ctx,
		q,
		query,
		time.Now().Add(-cfg.MinAge),
		time.Now().Add(-cfg.RejudgeInterval),
		cfg.BatchSize,
	)
}

func queryCandidates(
	ctx context.Context,
	q candidateQuerier,
	query string,
	args ...any,
) ([]candidate, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.infoHash, &c.name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func claimSyncCandidate(
	ctx context.Context,
	pool *pgxpool.Pool,
	infoHash []byte,
	torrentName, owner string,
	ttl time.Duration,
	cfg Config,
) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Batch reservation and synchronous admission share a short transaction-
	// scoped gate. A row-lock wait alone is insufficient: PostgreSQL can
	// evaluate NOT EXISTS(sync_claim) on a pre-wait snapshot and not rerun the
	// subquery after it acquires the torrent row. Taking this lock before the
	// candidate query forces the later mode to start from a post-commit
	// snapshot, while the per-torrent claim remains the long-lived ownership.
	if _, err := tx.Exec(ctx, batchAdmissionLockSQL); err != nil {
		return false, fmt.Errorf(
			"junkpurge sync: acquire admission lock: %w", err,
		)
	}

	var currentName string
	err = tx.QueryRow(ctx, `
SELECT name
FROM torrents
WHERE info_hash=$1
FOR UPDATE`, infoHash).Scan(&currentName)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && currentName != torrentName) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var claimed bool
	err = tx.QueryRow(ctx, `
INSERT INTO junkpurge_sync_claims (
  info_hash, owner, torrent_name, claimed_at, lease_until
)
SELECT t.info_hash,$2,t.name,now(),now()+make_interval(secs => $4)
FROM torrents t
WHERE t.info_hash=$1
  AND t.private = false
  AND t.name=$3
  AND EXISTS (
    SELECT 1 FROM torrent_contents tc
    WHERE tc.info_hash=t.info_hash
      AND tc.content_type IN ('movie','tv_show')
      AND tc.content_id IS NULL
      AND tc.created_at < $5
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_contents m
    WHERE m.info_hash=t.info_hash AND m.content_id IS NOT NULL
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_canonical_labels l WHERE l.info_hash=t.info_hash
  )
  AND NOT EXISTS (
    SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash
  )
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_judgments j
    WHERE j.info_hash=t.info_hash AND j.judged_at >= $6
  )
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_batch_items bi
    WHERE bi.info_hash=t.info_hash
      AND (
        bi.state IN ('pending','submitted','succeeded','retryable_error')
        OR (bi.state='abandoned' AND bi.retry_after > now())
      )
  )
ON CONFLICT (info_hash) DO UPDATE SET
  owner=excluded.owner,
  torrent_name=excluded.torrent_name,
  claimed_at=excluded.claimed_at,
  lease_until=excluded.lease_until
WHERE junkpurge_sync_claims.lease_until <= now()
RETURNING true`,
		infoHash, owner, torrentName, int64(ttl/time.Second),
		time.Now().Add(-cfg.MinAge), time.Now().Add(-cfg.RejudgeInterval),
	).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return claimed, nil
}

// settleSyncClaims closes out one cycle's claims. Three outcomes:
//
//   - judged     -> lease extended by rejudgeInterval (do not re-spend on a
//     title we already have a valid verdict for)
//   - cooled     -> lease set to now()+failureCooldown. The item was claimed
//     and the judge REJECTED its reply, so it has no verdict and must be
//     retried — but not on the next cycle. Deleting the claim here is what
//     made one permanently-malformed reply cost a call every hour forever
//
// ; the batch path already solved this with retry_after.
//   - neither    -> claim deleted, item immediately eligible again (the normal
//     path for an item we never got to)
func settleSyncClaims(
	ctx context.Context,
	pool *pgxpool.Pool,
	owner string,
	claimed, judged, cooled [][]byte,
	rejudgeInterval, failureCooldown time.Duration,
) error {
	if len(claimed) == 0 {
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if len(judged) > 0 {
		if _, err := tx.Exec(ctx, `
UPDATE junkpurge_sync_claims
SET lease_until=greatest(
      lease_until,
      now()+make_interval(secs => $3)
    )
WHERE owner=$1 AND info_hash=ANY($2)`,
			// .Seconds(), not int64(d/time.Second): make_interval's secs is
			// double precision, and the integer division floors any sub-second
			// remainder to 0 — an accepted positive duration would then extend
			// the lease by nothing at all.
			owner, judged, rejudgeInterval.Seconds(),
		); err != nil {
			return err
		}
	}
	if len(cooled) > 0 {
		// greatest() so a concurrent longer lease is never shortened.
		if _, err := tx.Exec(ctx, `
UPDATE junkpurge_sync_claims
SET lease_until=greatest(
      lease_until,
      now()+make_interval(secs => $3)
    )
WHERE owner=$1 AND info_hash=ANY($2)`,
			owner, cooled, failureCooldown.Seconds(),
		); err != nil {
			return err
		}
	}
	// Anything claimed that got neither a verdict nor a cooldown is released
	// for immediate re-selection. Concatenating rather than passing two arrays
	// keeps this a single NOT(ANY(...)) predicate.
	keep := make([][]byte, 0, len(judged)+len(cooled))
	keep = append(keep, judged...)
	keep = append(keep, cooled...)
	if len(keep) == 0 {
		if _, err := tx.Exec(ctx, `
DELETE FROM junkpurge_sync_claims
WHERE owner=$1 AND info_hash=ANY($2)`,
			owner, claimed,
		); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM junkpurge_sync_claims
WHERE owner=$1
  AND info_hash=ANY($2)
  AND NOT (info_hash=ANY($3))`,
		owner, claimed, keep,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func recordClaimedJudgment(
	ctx context.Context,
	pool *pgxpool.Pool,
	infoHash []byte,
	judgment Judgment,
	torrentName, owner string,
	cfg Config,
) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// A valid LLM reply can arrive minutes after admission. Establish a fresh,
	// locked view before it enters the operator's dry-run drop list.
	if _, err := tx.Exec(ctx, `
LOCK TABLE label_evidence, torrent_canonical_labels IN SHARE MODE`); err != nil {
		return false, err
	}
	var owned bool
	err = tx.QueryRow(ctx, `
SELECT true
FROM torrents t
JOIN junkpurge_sync_claims sc ON sc.info_hash=t.info_hash
WHERE t.info_hash=$1
  AND t.private = false
  AND t.name=$2
  AND sc.owner=$3
  AND sc.lease_until > now()
  AND sc.torrent_name=t.name
FOR UPDATE OF t, sc`,
		infoHash, torrentName, owner,
	).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	contentRows, err := tx.Query(ctx, `
SELECT info_hash
FROM torrent_contents
WHERE info_hash=$1
FOR UPDATE`, infoHash)
	if err != nil {
		return false, err
	}
	for contentRows.Next() {
		var ignored []byte
		if err := contentRows.Scan(&ignored); err != nil {
			contentRows.Close()
			return false, err
		}
	}
	contentRows.Close()
	if err := contentRows.Err(); err != nil {
		return false, err
	}

	var eligible bool
	err = tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1
  FROM torrents t
  JOIN junkpurge_sync_claims sc ON sc.info_hash=t.info_hash
  WHERE t.info_hash=$1
    AND t.private = false
    AND t.name=$2
    AND sc.owner=$3
    AND sc.lease_until > now()
    AND sc.torrent_name=t.name
    AND EXISTS (
      SELECT 1 FROM torrent_contents tc
      WHERE tc.info_hash=t.info_hash
        AND tc.content_type IN ('movie','tv_show')
        AND tc.content_id IS NULL
        AND tc.created_at < now()-make_interval(secs => $4)
    )
    AND NOT EXISTS (
      SELECT 1 FROM torrent_contents m
      WHERE m.info_hash=t.info_hash AND m.content_id IS NOT NULL
    )
    AND NOT EXISTS (
      SELECT 1 FROM torrent_canonical_labels l WHERE l.info_hash=t.info_hash
    )
    AND NOT EXISTS (
      SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash
    )
    AND NOT EXISTS (
      SELECT 1 FROM junkpurge_judgments j
      WHERE j.info_hash=t.info_hash AND j.judged_at > sc.claimed_at
    )
    AND NOT EXISTS (
      SELECT 1 FROM junkpurge_batch_items bi
      WHERE bi.info_hash=t.info_hash
        AND (
          bi.state IN ('pending','submitted','succeeded','retryable_error')
          OR (bi.state='abandoned' AND bi.retry_after > now())
        )
    )
)`,
		infoHash, torrentName, owner, int64(cfg.MinAge/time.Second),
	).Scan(&eligible)
	if err != nil {
		return false, err
	}
	if !eligible {
		return false, nil
	}

	var recorded bool
	err = tx.QueryRow(ctx, `
INSERT INTO junkpurge_judgments (
  info_hash, verdict, confidence, reason, torrent_name, judged_at, purged
) VALUES ($1,$2,$3,$6,$4,now(),false)
ON CONFLICT (info_hash) DO UPDATE SET
  verdict=excluded.verdict,
  confidence=excluded.confidence,
  reason=excluded.reason,
  torrent_name=excluded.torrent_name,
  judged_at=excluded.judged_at,
  purged=false
WHERE junkpurge_judgments.judged_at <= (
  SELECT claimed_at
  FROM junkpurge_sync_claims
  WHERE info_hash=$1 AND owner=$5
)
RETURNING true`,
		infoHash, judgment.Verdict, judgment.Confidence, torrentName, owner,
		judgmentReasonSyncPending,
	).Scan(&recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return recorded, nil
}

func releaseSyncClaims(
	ctx context.Context,
	pool *pgxpool.Pool,
	owner string,
	infoHashes [][]byte,
) error {
	if len(infoHashes) == 0 {
		return nil
	}
	_, err := pool.Exec(ctx, `
DELETE FROM junkpurge_sync_claims
WHERE owner=$1 AND info_hash=ANY($2)`, owner, infoHashes)
	return err
}

func eligibleSyncJunkTx(
	ctx context.Context,
	tx pgx.Tx,
	infoHashes [][]byte,
	minConfidence float64,
	minAge time.Duration,
	claimOwner string,
) ([][]byte, error) {
	if len(infoHashes) == 0 {
		return nil, nil
	}
	// Evidence rows have no FK to torrents, so block their writers while
	// eligibility is rechecked and the quarantine snapshot/delete commits.
	if _, err := tx.Exec(ctx, `
LOCK TABLE label_evidence, torrent_canonical_labels IN SHARE MODE`); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE junkpurge_sync_claims
SET lease_until=now()+interval '30 minutes'
WHERE owner=$1 AND info_hash=ANY($2) AND lease_until > now()`,
		claimOwner, infoHashes,
	); err != nil {
		return nil, err
	}

	// First lock the complete ownership chain, but defer content eligibility
	// until after its child rows are locked. Evaluating content predicates
	// before their locks are acquired can return a stale pre-update snapshot.
	rows, err := tx.Query(ctx, `
SELECT t.info_hash
FROM torrents t
JOIN junkpurge_sync_claims sc ON sc.info_hash=t.info_hash
JOIN junkpurge_judgments j ON j.info_hash=t.info_hash
WHERE t.info_hash=ANY($1)
  AND t.private = false
  AND sc.owner=$2
  AND sc.lease_until > now()
  AND sc.torrent_name=t.name
  AND j.verdict='junk'
  AND j.confidence >= $3
  AND j.torrent_name=t.name
FOR UPDATE OF t, sc, j`,
		infoHashes, claimOwner, minConfidence,
	)
	if err != nil {
		return nil, err
	}
	var locked [][]byte
	for rows.Next() {
		var hash []byte
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return nil, err
		}
		locked = append(locked, hash)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(locked) == 0 {
		return nil, nil
	}

	// Parent rows are now locked; lock existing child rows as well. The FK
	// makes a new child insert wait on the parent until settlement commits.
	contentRows, err := tx.Query(ctx, `
SELECT info_hash
FROM torrent_contents
WHERE info_hash=ANY($1)
FOR UPDATE`, locked)
	if err != nil {
		return nil, err
	}
	for contentRows.Next() {
		var ignored []byte
		if err := contentRows.Scan(&ignored); err != nil {
			contentRows.Close()
			return nil, err
		}
	}
	contentRows.Close()
	if err := contentRows.Err(); err != nil {
		return nil, err
	}

	// Re-evaluate only after all mutable rows involved in the decision are
	// locked. The table locks above keep label/evidence inserts out until this
	// transaction either snapshots and deletes the torrent or rolls back.
	rows, err = tx.Query(ctx, `
SELECT t.info_hash
FROM torrents t
JOIN junkpurge_sync_claims sc ON sc.info_hash=t.info_hash
JOIN junkpurge_judgments j ON j.info_hash=t.info_hash
WHERE t.info_hash=ANY($1)
  AND t.private = false
  AND sc.owner=$2
  AND sc.lease_until > now()
  AND sc.torrent_name=t.name
  AND j.verdict='junk'
  AND j.confidence >= $3
  AND j.torrent_name=t.name
  AND EXISTS (
    SELECT 1 FROM torrent_contents tc
    WHERE tc.info_hash=t.info_hash
      AND tc.content_type IN ('movie','tv_show')
      AND tc.content_id IS NULL
      AND tc.created_at < now()-make_interval(secs => $4)
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_contents m
    WHERE m.info_hash=t.info_hash AND m.content_id IS NOT NULL
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_canonical_labels l WHERE l.info_hash=t.info_hash
  )
  AND NOT EXISTS (
    SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash
  )
ORDER BY t.info_hash`,
		locked, claimOwner, minConfidence, int64(minAge/time.Second),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var eligible [][]byte
	for rows.Next() {
		var hash []byte
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		eligible = append(eligible, hash)
	}
	return eligible, rows.Err()
}

func countActiveBatchRuns(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var count int
	err := pool.QueryRow(ctx, `
SELECT count(*)
FROM junkpurge_batch_runs
WHERE state IN ('building','active','finalizing')`).Scan(&count)
	return count, err
}

func requireCaptureOnlyBatchIdle(
	ctx context.Context,
	pool *pgxpool.Pool,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf(
			"junkpurge capture-only begin Batch preflight: %w",
			err,
		)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize this check with createBatchRun across replicas. The lock is
	// transaction-scoped, so operators must still avoid a mixed rolling
	// deployment where an older Batch-enabled replica remains after preflight.
	if _, err := tx.Exec(ctx, batchAdmissionLockSQL); err != nil {
		return fmt.Errorf(
			"junkpurge capture-only acquire Batch admission lock: %w",
			err,
		)
	}
	var active int
	if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM junkpurge_batch_runs
WHERE state IN ('building','active','finalizing')`).Scan(&active); err != nil {
		return fmt.Errorf(
			"junkpurge capture-only count active Batch runs: %w",
			err,
		)
	}
	if active != 0 {
		return fmt.Errorf(
			"junkpurge capture-only requires zero active Batch runs; found %d; disable capture and let the ordinary drain settle them first",
			active,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf(
			"junkpurge capture-only commit Batch preflight: %w",
			err,
		)
	}
	return nil
}

func listRunsNeedingAttempt(ctx context.Context, pool *pgxpool.Pool, limit int) ([]batchRun, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
SELECT r.id, r.state, r.model, r.provider_base_url, r.endpoint,
       r.prompt_version, r.system_prompt,
       r.max_completion_tokens, r.reasoning_effort, r.completion_window,
       r.min_age_seconds, r.min_confidence,
       r.max_junk_rate, r.enable_purge, r.quarantine_days,
       r.failure_cooldown_seconds, r.max_attempts,
       r.ambiguity_grace_seconds, r.item_count,
       r.created_at
FROM junkpurge_batch_runs r
WHERE r.state = 'active'
  AND EXISTS (
    SELECT 1 FROM junkpurge_batch_items i
    WHERE i.run_id=r.id AND i.state IN ('pending','retryable_error')
  )
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_batch_attempts a
    WHERE a.run_id=r.id AND a.ingested_at IS NULL
  )
ORDER BY r.created_at
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []batchRun
	for rows.Next() {
		var run batchRun
		var minAgeSeconds, cooldownSeconds, graceSeconds int64
		if err := rows.Scan(
			&run.ID, &run.State, &run.Model, &run.ProviderBaseURL, &run.Endpoint,
			&run.PromptVersion, &run.SystemPrompt, &run.MaxTokens,
			&run.ReasoningEffort, &run.CompletionWindow, &minAgeSeconds,
			&run.MinConfidence, &run.MaxJunkRate, &run.EnablePurge,
			&run.QuarantineDays, &cooldownSeconds, &run.MaxAttempts,
			&graceSeconds, &run.ItemCount, &run.CreatedAt,
		); err != nil {
			return nil, err
		}
		run.MinAge = time.Duration(minAgeSeconds) * time.Second
		run.FailureCooldown = time.Duration(cooldownSeconds) * time.Second
		run.AmbiguityGrace = time.Duration(graceSeconds) * time.Second
		out = append(out, run)
	}
	return out, rows.Err()
}

func prepareBatchAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	run batchRun,
	build batchPayloadBuilder,
) (*batchAttempt, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	if err := tx.QueryRow(ctx,
		`SELECT state FROM junkpurge_batch_runs WHERE id=$1 FOR UPDATE`,
		run.ID,
	).Scan(&state); err != nil {
		return nil, err
	}
	if state != runStateActive {
		return nil, nil
	}

	var unsettled bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1
  FROM junkpurge_batch_attempts
  WHERE run_id=$1 AND ingested_at IS NULL
)`, run.ID).Scan(&unsettled); err != nil {
		return nil, err
	}
	if unsettled {
		return nil, nil
	}

	var attemptNo int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(max(attempt_no),0)+1 FROM junkpurge_batch_attempts WHERE run_id=$1`,
		run.ID,
	).Scan(&attemptNo); err != nil {
		return nil, err
	}
	if attemptNo > run.MaxAttempts {
		if err := failBatchRunTx(ctx, tx, run.ID, "retry_exhausted",
			"provider Batch retry ceiling reached"); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}

	// label_evidence and torrent_canonical_labels deliberately have no FK to
	// torrents. Hold SHARE while the candidate rows are locked, rechecked, and
	// serialized so a concurrent qB poll/evidence resolver cannot cross the
	// final local egress gate unnoticed.
	if _, err := tx.Exec(ctx, `
LOCK TABLE label_evidence, torrent_canonical_labels IN SHARE MODE`); err != nil {
		return nil, fmt.Errorf("junkpurge batch: lock pre-upload evidence: %w", err)
	}

	rows, err := tx.Query(ctx, `
SELECT run_id, ordinal, custom_id, info_hash, torrent_name, state, attempt_count
FROM junkpurge_batch_items
WHERE run_id=$1
  AND state IN ('pending','retryable_error')
  AND attempt_count < $2
ORDER BY ordinal
FOR UPDATE`, run.ID, run.MaxAttempts)
	if err != nil {
		return nil, err
	}
	var items []batchItem
	for rows.Next() {
		var item batchItem
		if err := rows.Scan(
			&item.RunID, &item.Ordinal, &item.CustomID, &item.InfoHash,
			&item.TorrentName, &item.State, &item.AttemptCount,
		); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	// Lock every reserved torrent and its existing content rows before the
	// eligibility query. A native-private/name update or content attachment
	// that was already in flight becomes visible after the wait; later updates
	// cannot commit until the payload transaction is complete.
	if err := lockBatchAttemptCandidateRowsTx(ctx, tx, run.ID); err != nil {
		return nil, err
	}
	items, excluded, err := filterBatchAttemptItemsTx(ctx, tx, run, items)
	if err != nil {
		return nil, err
	}
	if excluded > 0 {
		tag, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_runs
SET item_count=item_count-$2, updated_at=now()
WHERE id=$1 AND state='active' AND item_count >= $2`,
			run.ID, excluded,
		)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() != 1 {
			return nil, errors.New(
				"junkpurge batch: pre-upload exclusion lost run lock",
			)
		}
		run.ItemCount -= excluded
	}
	if len(items) == 0 {
		tag, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_runs
SET state='finalizing', updated_at=now()
WHERE id=$1
  AND state='active'
  AND NOT EXISTS (
    SELECT 1 FROM junkpurge_batch_items
    WHERE run_id=$1 AND state IN ('pending','retryable_error','submitted')
  )`, run.ID)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() != 1 {
			return nil, errors.New(
				"junkpurge batch: empty pre-upload cohort could not finalize",
			)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}

	payload, filename, sha256, err := build(run, attemptNo, items)
	if err != nil {
		return nil, err
	}
	attempt := batchAttempt{
		RunID:            run.ID,
		AttemptNo:        attemptNo,
		State:            "prepared",
		InputFilename:    filename,
		InputSHA256:      sha256,
		InputBytes:       int64(len(payload)),
		ItemCount:        len(items),
		Payload:          payload,
		Endpoint:         run.Endpoint,
		CompletionWindow: run.CompletionWindow,
		MaxAttempts:      run.MaxAttempts,
		AmbiguityGrace:   run.AmbiguityGrace,
		ProviderBaseURL:  run.ProviderBaseURL,
	}
	err = tx.QueryRow(ctx, `
INSERT INTO junkpurge_batch_attempts (
  run_id, attempt_no, state, input_filename, input_sha256,
  input_payload, input_bytes, item_count, next_attempt_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())
RETURNING id`,
		attempt.RunID, attempt.AttemptNo, attempt.State,
		attempt.InputFilename, attempt.InputSHA256, attempt.Payload,
		attempt.InputBytes, attempt.ItemCount,
	).Scan(&attempt.ID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		tag, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_items
SET state='submitted', attempt_count=attempt_count+1,
    last_attempt_id=$3, updated_at=now()
WHERE run_id=$1 AND ordinal=$2
  AND state IN ('pending','retryable_error')`,
			item.RunID, item.Ordinal, attempt.ID,
		)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() != 1 {
			return nil, fmt.Errorf("junkpurge batch: item %s changed while preparing attempt",
				item.CustomID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &attempt, nil
}

func lockBatchAttemptCandidateRowsTx(
	ctx context.Context,
	tx pgx.Tx,
	runID int64,
) error {
	rows, err := tx.Query(ctx, `
SELECT t.info_hash
FROM torrents t
JOIN junkpurge_batch_items i ON i.info_hash=t.info_hash
WHERE i.run_id=$1 AND i.state IN ('pending','retryable_error')
ORDER BY i.ordinal
FOR UPDATE OF t`, runID)
	if err != nil {
		return fmt.Errorf("junkpurge batch: lock pre-upload torrents: %w", err)
	}
	for rows.Next() {
		var ignored []byte
		if err := rows.Scan(&ignored); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = tx.Query(ctx, `
SELECT tc.info_hash
FROM torrent_contents tc
JOIN junkpurge_batch_items i ON i.info_hash=tc.info_hash
WHERE i.run_id=$1 AND i.state IN ('pending','retryable_error')
ORDER BY i.ordinal
FOR UPDATE OF tc`, runID)
	if err != nil {
		return fmt.Errorf("junkpurge batch: lock pre-upload contents: %w", err)
	}
	for rows.Next() {
		var ignored []byte
		if err := rows.Scan(&ignored); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func filterBatchAttemptItemsTx(
	ctx context.Context,
	tx pgx.Tx,
	run batchRun,
	items []batchItem,
) ([]batchItem, int, error) {
	rows, err := tx.Query(
		ctx,
		batchAttemptEligibilityQuery,
		run.ID,
		int64(run.MinAge/time.Second),
		run.CreatedAt,
	)
	if err != nil {
		return nil, 0, fmt.Errorf(
			"junkpurge batch: pre-upload eligibility query: %w", err,
		)
	}
	eligible := make(map[string]struct{}, len(items))
	for rows.Next() {
		var customID string
		if err := rows.Scan(&customID); err != nil {
			rows.Close()
			return nil, 0, err
		}
		eligible[customID] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	kept := make([]batchItem, 0, len(items))
	excludedIDs := make([]string, 0)
	for _, item := range items {
		if _, ok := eligible[item.CustomID]; ok {
			kept = append(kept, item)
			continue
		}
		excludedIDs = append(excludedIDs, item.CustomID)
	}
	if len(excludedIDs) == 0 {
		return kept, 0, nil
	}

	tag, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_items
SET state='abandoned',
    error_code='pre_upload_ineligible',
    error_message='excluded by final local privacy/eligibility gate before provider payload construction',
    retry_after=NULL, completed_at=now(), updated_at=now()
WHERE run_id=$1
  AND custom_id=ANY($2::text[])
  AND state IN ('pending','retryable_error')`,
		run.ID, excludedIDs,
	)
	if err != nil {
		return nil, 0, err
	}
	if tag.RowsAffected() != int64(len(excludedIDs)) {
		return nil, 0, errors.New(
			"junkpurge batch: pre-upload exclusion lost item lock",
		)
	}
	return kept, len(excludedIDs), nil
}

func listUningestedAttempts(ctx context.Context, pool *pgxpool.Pool, limit int) ([]batchAttempt, error) {
	rows, err := pool.Query(ctx, `
SELECT a.id, a.run_id, a.attempt_no, a.state, a.input_filename, a.input_sha256,
       a.input_payload, a.input_bytes, a.item_count, coalesce(a.input_file_id,''),
       coalesce(a.provider_batch_id,''), coalesce(a.output_file_id,''),
       coalesce(a.error_file_id,''), coalesce(a.provider_status,''),
       a.request_total, a.request_completed, a.request_failed,
       a.submission_attempted_at, a.submitted_at, a.terminal_at, a.ingested_at,
       coalesce(a.last_error,''), r.endpoint, r.completion_window,
       r.max_attempts, r.ambiguity_grace_seconds, r.provider_base_url,
       r.model
FROM junkpurge_batch_attempts a
JOIN junkpurge_batch_runs r ON r.id=a.run_id
WHERE a.ingested_at IS NULL
  AND (a.lease_until IS NULL OR a.lease_until < now())
  AND (a.next_attempt_at IS NULL OR a.next_attempt_at <= now())
ORDER BY a.created_at
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []batchAttempt
	for rows.Next() {
		attempt, err := scanBatchAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, attempt)
	}
	return out, rows.Err()
}

type batchAttemptScanner interface {
	Scan(...any) error
}

func scanBatchAttempt(scanner batchAttemptScanner) (batchAttempt, error) {
	var attempt batchAttempt
	var ambiguityGraceSeconds int64
	err := scanner.Scan(
		&attempt.ID, &attempt.RunID, &attempt.AttemptNo, &attempt.State,
		&attempt.InputFilename, &attempt.InputSHA256, &attempt.Payload,
		&attempt.InputBytes, &attempt.ItemCount, &attempt.InputFileID,
		&attempt.ProviderBatchID, &attempt.OutputFileID, &attempt.ErrorFileID,
		&attempt.ProviderStatus, &attempt.RequestTotal, &attempt.RequestCompleted,
		&attempt.RequestFailed, &attempt.SubmissionAttemptedAt, &attempt.SubmittedAt,
		&attempt.TerminalAt, &attempt.IngestedAt, &attempt.LastError,
		&attempt.Endpoint, &attempt.CompletionWindow, &attempt.MaxAttempts,
		&ambiguityGraceSeconds, &attempt.ProviderBaseURL, &attempt.Model,
	)
	attempt.AmbiguityGrace = time.Duration(ambiguityGraceSeconds) * time.Second
	return attempt, err
}

func loadBatchAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
) (batchAttempt, error) {
	row := pool.QueryRow(ctx, `
SELECT a.id, a.run_id, a.attempt_no, a.state, a.input_filename, a.input_sha256,
       a.input_payload, a.input_bytes, a.item_count, coalesce(a.input_file_id,''),
       coalesce(a.provider_batch_id,''), coalesce(a.output_file_id,''),
       coalesce(a.error_file_id,''), coalesce(a.provider_status,''),
       a.request_total, a.request_completed, a.request_failed,
       a.submission_attempted_at, a.submitted_at, a.terminal_at, a.ingested_at,
       coalesce(a.last_error,''), r.endpoint, r.completion_window,
       r.max_attempts, r.ambiguity_grace_seconds, r.provider_base_url,
       r.model
FROM junkpurge_batch_attempts a
JOIN junkpurge_batch_runs r ON r.id=a.run_id
WHERE a.id=$1`, id)
	return scanBatchAttempt(row)
}

func claimBatchAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
	owner string,
	ttl time.Duration,
) (bool, error) {
	var claimed bool
	err := pool.QueryRow(ctx, `
UPDATE junkpurge_batch_attempts
SET lease_owner=$2,
    lease_until=now()+make_interval(secs => $3),
    updated_at=now()
WHERE id=$1
  AND ingested_at IS NULL
  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
  AND (lease_until IS NULL OR lease_until < now() OR lease_owner=$2)
RETURNING true`,
		id, owner, int64(ttl/time.Second),
	).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return claimed, err
}

func acquireBatchAttemptAdvisoryLock(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
) (*pgxpool.Conn, bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var locked bool
	err = conn.QueryRow(ctx, `
SELECT pg_try_advisory_lock(
  hashtextextended('bitagent:junkpurge:attempt:' || $1::text, 0)
)`, id).Scan(&locked)
	if err != nil || !locked {
		conn.Release()
		return nil, false, err
	}
	return conn, locked, err
}

func releaseBatchAttemptAdvisoryLock(
	ctx context.Context,
	conn *pgxpool.Conn,
	id int64,
) error {
	var unlocked bool
	err := conn.QueryRow(ctx, `
SELECT pg_advisory_unlock(
  hashtextextended('bitagent:junkpurge:attempt:' || $1::text, 0)
)`, id).Scan(&unlocked)
	if err == nil && unlocked {
		conn.Release()
		return nil
	}
	// Never return a session that may still hold the advisory lock to the
	// pool. Hijacking and closing forces PostgreSQL to release all locks even
	// when the explicit unlock query timed out or the connection went bad.
	raw := conn.Hijack()
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = raw.Close(closeCtx)
	if err != nil {
		return err
	}
	return errors.New("junkpurge batch: attempt advisory lock was not held")
}

func releaseBatchAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
	owner string,
) error {
	_, err := pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET lease_owner=NULL, lease_until=NULL, updated_at=now()
WHERE id=$1 AND lease_owner=$2`, id, owner)
	return err
}

func loadAttemptItems(
	ctx context.Context,
	pool *pgxpool.Pool,
	attemptID int64,
) ([]batchItem, error) {
	rows, err := pool.Query(ctx, `
SELECT run_id, ordinal, custom_id, info_hash, torrent_name, state, attempt_count
FROM junkpurge_batch_items
WHERE last_attempt_id=$1
ORDER BY ordinal`, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []batchItem
	for rows.Next() {
		var item batchItem
		if err := rows.Scan(
			&item.RunID, &item.Ordinal, &item.CustomID, &item.InfoHash,
			&item.TorrentName, &item.State, &item.AttemptCount,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func batchAttemptPrivacySafe(
	ctx context.Context,
	pool *pgxpool.Pool,
	attemptID int64,
	expectedItems int,
	leaseOwner string,
) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// This check protects persisted manifests created by an older binary as
	// well as newly prepared attempts. Lock append-only qB evidence while the
	// represented torrent rows are re-read so an already-in-flight privacy
	// observation is visible and a later one cannot commit across the check.
	if _, err := tx.Exec(ctx, `LOCK TABLE label_evidence IN SHARE MODE`); err != nil {
		return false, fmt.Errorf(
			"junkpurge batch: lock provider-boundary evidence: %w", err,
		)
	}
	var active bool
	err = tx.QueryRow(ctx, `
SELECT ingested_at IS NULL AND provider_batch_id IS NULL
FROM junkpurge_batch_attempts
WHERE id=$1 AND lease_owner=$2
FOR UPDATE`,
		attemptID, leaseOwner,
	).Scan(&active)
	if err != nil {
		return false, err
	}
	if !active {
		return false, errors.New(
			"junkpurge batch: provider-boundary privacy check lost attempt ownership",
		)
	}

	rows, err := tx.Query(ctx, `
SELECT t.info_hash
FROM torrents t
JOIN junkpurge_batch_items i ON i.info_hash=t.info_hash
WHERE i.last_attempt_id=$1
ORDER BY i.ordinal
FOR UPDATE OF t`, attemptID)
	if err != nil {
		return false, fmt.Errorf(
			"junkpurge batch: lock provider-boundary torrents: %w", err,
		)
	}
	for rows.Next() {
		var ignored []byte
		if err := rows.Scan(&ignored); err != nil {
			rows.Close()
			return false, err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}

	var represented, unsafe int
	err = tx.QueryRow(ctx, `
SELECT
  count(*),
  count(*) FILTER (
    WHERE t.info_hash IS NULL
       OR t.private
       OR EXISTS (
         SELECT 1 FROM label_evidence private_evidence
         WHERE private_evidence.info_hash=i.info_hash
           AND private_evidence.source='qbittorrent'
           AND lower(private_evidence.category) IN ('private','bitgrab')
       )
  )
FROM junkpurge_batch_items i
LEFT JOIN torrents t ON t.info_hash=i.info_hash
WHERE i.last_attempt_id=$1`,
		attemptID,
	).Scan(&represented, &unsafe)
	if err != nil {
		return false, err
	}
	if represented != expectedItems {
		return false, fmt.Errorf(
			"junkpurge batch: provider-boundary manifest has %d/%d represented items",
			represented, expectedItems,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return unsafe == 0, nil
}

func setAttemptUploading(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
	owner string,
) error {
	tag, err := pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state='uploading', submission_attempted_at=now(),
    next_attempt_at=NULL, updated_at=now()
WHERE id=$1 AND input_file_id IS NULL AND lease_owner=$2`, id, owner)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("junkpurge batch: attempt %d upload transition lost ownership", id)
	}
	return err
}

func setAttemptInputFile(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
	fileID string,
	owner string,
) error {
	tag, err := pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state='uploaded', input_file_id=$2, last_error=NULL,
    next_attempt_at=NULL, updated_at=now()
WHERE id=$1 AND provider_batch_id IS NULL AND lease_owner=$3`,
		id, fileID, owner)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("junkpurge batch: attempt %d file transition lost ownership", id)
	}
	return err
}

func setAttemptSubmitting(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
	owner string,
) error {
	tag, err := pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state='submitting', submission_attempted_at=now(),
    next_attempt_at=NULL, updated_at=now()
WHERE id=$1 AND provider_batch_id IS NULL AND lease_owner=$2`, id, owner)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("junkpurge batch: attempt %d submit transition lost ownership", id)
	}
	return err
}

func setAttemptProviderBatch(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
	providerID, status string,
	submittedAt time.Time,
	owner string,
) error {
	tag, err := pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state=$3, provider_batch_id=$2, provider_status=$3,
    submitted_at=$4, last_error=NULL, next_attempt_at=NULL, updated_at=now()
WHERE id=$1 AND lease_owner=$5`,
		id, providerID, status, submittedAt, owner)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("junkpurge batch: attempt %d provider transition lost ownership", id)
	}
	return err
}

func updateAttemptProvider(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
	status, outputFileID, errorFileID string,
	total, completed, failed int,
	terminal bool,
	owner string,
) error {
	terminalAt := any(nil)
	if terminal {
		terminalAt = time.Now()
	}
	tag, err := pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state=$2, provider_status=$2,
    output_file_id=coalesce(nullif($3,''),output_file_id),
    error_file_id=coalesce(nullif($4,''),error_file_id), request_total=$5,
    request_completed=$6, request_failed=$7,
    terminal_at=coalesce(terminal_at,$8), last_error=NULL,
    next_attempt_at=NULL, updated_at=now()
WHERE id=$1 AND lease_owner=$9`,
		id, status, outputFileID, errorFileID, total, completed, failed,
		terminalAt, owner,
	)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("junkpurge batch: attempt %d status transition lost ownership", id)
	}
	return err
}

func setAttemptError(
	ctx context.Context,
	pool *pgxpool.Pool,
	id int64,
	message string,
	owner string,
) error {
	tag, err := pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET last_error=$2, next_attempt_at=now()+interval '1 minute', updated_at=now()
WHERE id=$1 AND lease_owner=$3`, id, message, owner)
	if err == nil && tag.RowsAffected() != 1 {
		return fmt.Errorf("junkpurge batch: attempt %d error transition lost ownership", id)
	}
	return err
}

func ingestBatchAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	attempt batchAttempt,
	providerTerminalStatus string,
	results []batchItemResult,
	leaseOwner string,
) (batchIngestSummary, error) {
	summary := batchIngestSummary{
		RunID:     attempt.RunID,
		AttemptID: attempt.ID,
		AttemptNo: attempt.AttemptNo,
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return summary, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ingestedAt *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT ingested_at
		 FROM junkpurge_batch_attempts
		 WHERE id=$1 AND lease_owner=$2
		 FOR UPDATE`,
		attempt.ID, leaseOwner,
	).Scan(&ingestedAt); err != nil {
		return summary, err
	}
	if ingestedAt != nil {
		return summary, nil
	}

	expectedRows, err := tx.Query(ctx, `
SELECT custom_id
FROM junkpurge_batch_items
WHERE last_attempt_id=$1 AND state='submitted'
FOR UPDATE`, attempt.ID)
	if err != nil {
		return summary, err
	}
	expected := make(map[string]struct{}, attempt.ItemCount)
	for expectedRows.Next() {
		var customID string
		if err := expectedRows.Scan(&customID); err != nil {
			expectedRows.Close()
			return summary, err
		}
		expected[customID] = struct{}{}
	}
	expectedRows.Close()
	if err := expectedRows.Err(); err != nil {
		return summary, err
	}

	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		if _, ok := expected[result.CustomID]; !ok {
			return summary, fmt.Errorf("junkpurge batch: unexpected custom_id %q", result.CustomID)
		}
		if _, duplicate := seen[result.CustomID]; duplicate {
			return summary, fmt.Errorf("junkpurge batch: duplicate custom_id %q", result.CustomID)
		}
		seen[result.CustomID] = struct{}{}

		state := itemStateTerminalError
		if result.Succeeded {
			state = itemStateSucceeded
			summary.Succeeded++
		} else if result.Retryable &&
			batchStatusAllowsItemRetry(providerTerminalStatus) {
			state = itemStateRetryableError
			summary.RetryableFailures++
		} else {
			summary.TerminalFailures++
		}
		summary.Usage.Input += result.Usage.Input
		summary.Usage.Cached += result.Usage.Cached
		summary.Usage.CacheWrite += result.Usage.CacheWrite
		summary.Usage.Output += result.Usage.Output
		summary.Usage.Reasoning += result.Usage.Reasoning

		tag, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_items
SET state=$3, verdict=nullif($4,''), confidence=$5,
    provider_request_id=nullif($6,''), response_status=nullif($7,0),
    error_code=nullif($8,''), error_message=nullif($9,''),
    input_tokens=input_tokens+$10,
    cached_input_tokens=cached_input_tokens+$11,
    cache_write_tokens=cache_write_tokens+$12,
    output_tokens=output_tokens+$13,
    reasoning_tokens=reasoning_tokens+$14,
    completed_at=now(), updated_at=now()
WHERE run_id=$1 AND custom_id=$2 AND last_attempt_id=$15
  AND state='submitted'`,
			attempt.RunID, result.CustomID, state, result.Judgment.Verdict,
			nullableConfidence(result), result.ProviderRequest,
			result.ResponseStatus, result.ErrorCode, result.ErrorMessage,
			result.Usage.Input, result.Usage.Cached, result.Usage.CacheWrite,
			result.Usage.Output, result.Usage.Reasoning, attempt.ID,
		)
		if err != nil {
			return summary, err
		}
		if tag.RowsAffected() != 1 {
			return summary, fmt.Errorf("junkpurge batch: result %q was already settled", result.CustomID)
		}
	}

	missingRetryable := missingBatchResultRetryable(providerTerminalStatus)
	for customID := range expected {
		if _, ok := seen[customID]; ok {
			continue
		}
		summary.Missing++
		state := itemStateTerminalError
		if missingRetryable {
			state = itemStateRetryableError
			summary.RetryableFailures++
		} else {
			summary.TerminalFailures++
		}
		if _, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_items
SET state=$3, error_code='missing_result',
    error_message='provider terminal response omitted this custom_id',
    completed_at=now(), updated_at=now()
WHERE run_id=$1 AND custom_id=$2 AND last_attempt_id=$4
  AND state='submitted'`,
			attempt.RunID, customID, state, attempt.ID,
		); err != nil {
			return summary, err
		}
	}

	if _, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state=$2, provider_status=$2, ingested_at=now(),
    terminal_at=coalesce(terminal_at,now()), updated_at=now()
WHERE id=$1`, attempt.ID, providerTerminalStatus); err != nil {
		return summary, err
	}

	switch {
	case providerTerminalStatus == "cancelled":
		if err := failBatchRunTx(ctx, tx, attempt.RunID, "provider_cancelled",
			"provider Batch was cancelled; automatic resubmission is disabled"); err != nil {
			return summary, err
		}
		summary.RunState = runStateFailed
	case providerTerminalStatus == "failed":
		if err := failBatchRunTx(ctx, tx, attempt.RunID, "provider_failed",
			"provider Batch failed validation or execution"); err != nil {
			return summary, err
		}
		summary.RunState = runStateFailed
	case summary.TerminalFailures > 0:
		if err := failBatchRunTx(ctx, tx, attempt.RunID, "terminal_item_error",
			"one or more provider items failed permanently"); err != nil {
			return summary, err
		}
		summary.RunState = runStateFailed
	case summary.RetryableFailures > 0 && attempt.AttemptNo >= attempt.MaxAttempts:
		if err := failBatchRunTx(ctx, tx, attempt.RunID, "retry_exhausted",
			"provider Batch retry ceiling reached"); err != nil {
			return summary, err
		}
		summary.RunState = runStateFailed
	case summary.RetryableFailures > 0:
		summary.RunState = runStateActive
	default:
		tag, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_runs
SET state='finalizing', updated_at=now()
WHERE id=$1 AND state='active'`, attempt.RunID)
		if err != nil {
			return summary, err
		}
		if tag.RowsAffected() != 1 {
			return summary, errors.New("junkpurge batch: run was not active at finalization")
		}
		summary.RunState = runStateFinalizing
	}
	if err := tx.Commit(ctx); err != nil {
		return summary, err
	}
	return summary, nil
}

func missingBatchResultRetryable(providerTerminalStatus string) bool {
	// Expiry is the provider-documented partial-completion case. A deliberate
	// operator cancellation must stop spend, and validation failure should
	// not automatically resubmit the same invalid manifest.
	return providerTerminalStatus == "expired"
}

func batchStatusAllowsItemRetry(providerTerminalStatus string) bool {
	return providerTerminalStatus == "completed" ||
		providerTerminalStatus == "expired"
}

func nullableConfidence(result batchItemResult) any {
	if !result.Succeeded {
		return nil
	}
	return result.Judgment.Confidence
}

func failBatchRunTx(
	ctx context.Context,
	tx pgx.Tx,
	runID int64,
	code, message string,
) error {
	if _, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_runs
SET state='failed', failure_code=$2, failure_message=$3,
    finalized_at=now(), updated_at=now()
WHERE id=$1 AND state IN ('building','active','finalizing')`,
		runID, code, message,
	); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_items
SET state='abandoned',
    retry_after=now()+make_interval(secs => (
      SELECT failure_cooldown_seconds
      FROM junkpurge_batch_runs
      WHERE id=$1
    )),
    updated_at=now()
WHERE run_id=$1
  AND state IN ('pending','submitted','succeeded','retryable_error','terminal_error')`,
		runID)
	return err
}

func failBatchRunsNeedingAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	limit int,
	code, message string,
) error {
	runs, err := listRunsNeedingAttempt(ctx, pool, limit)
	if err != nil {
		return err
	}
	var errs []error
	for _, run := range runs {
		tx, err := pool.Begin(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var active bool
		err = tx.QueryRow(ctx, `
SELECT state='active'
FROM junkpurge_batch_runs
WHERE id=$1
FOR UPDATE`, run.ID).Scan(&active)
		if err == nil && active {
			var unsettled bool
			err = tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM junkpurge_batch_attempts
  WHERE run_id=$1 AND ingested_at IS NULL
)`, run.ID).Scan(&unsettled)
			if err == nil && !unsettled {
				err = failBatchRunTx(ctx, tx, run.ID, code, message)
			}
		}
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("run %d: %w", run.ID, err))
		}
	}
	return errors.Join(errs...)
}

func failClaimedBatchAttempt(
	ctx context.Context,
	pool *pgxpool.Pool,
	attemptID int64,
	owner, code, message string,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var runID int64
	var ingestedAt *time.Time
	err = tx.QueryRow(ctx, `
SELECT run_id, ingested_at
FROM junkpurge_batch_attempts
WHERE id=$1 AND lease_owner=$2
FOR UPDATE`, attemptID, owner).Scan(&runID, &ingestedAt)
	if err != nil {
		return err
	}
	if ingestedAt != nil {
		return nil
	}
	tag, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state='failed', provider_status='not_submitted',
    terminal_at=now(), ingested_at=now(),
    last_error=$3, updated_at=now()
WHERE id=$1 AND lease_owner=$2 AND ingested_at IS NULL`,
		attemptID, owner, message)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf(
			"junkpurge batch: attempt %d disable transition lost ownership",
			attemptID,
		)
	}
	if err := failBatchRunTx(ctx, tx, runID, code, message); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// listBatchAttemptsPendingInputCleanup returns settled attempts that still hold
// a provider-hosted input file. A settled attempt never reuses its input (a
// retry uploads its own), so any remaining file is garbage the provider is
// still storing. Keeping the row eligible until the id is cleared is what makes
// deletion durable: a transient DELETE failure is retried on a later cycle
// instead of being lost with the terminal transition.
func listBatchAttemptsPendingInputCleanup(
	ctx context.Context,
	pool *pgxpool.Pool,
	limit int,
) ([]batchAttempt, error) {
	rows, err := pool.Query(ctx, `
SELECT id, run_id, attempt_no, state, coalesce(input_file_id,'')
FROM junkpurge_batch_attempts
WHERE state='failed'
  AND ingested_at IS NOT NULL
  AND input_file_id IS NOT NULL
ORDER BY id
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []batchAttempt
	for rows.Next() {
		var attempt batchAttempt
		if err := rows.Scan(
			&attempt.ID,
			&attempt.RunID,
			&attempt.AttemptNo,
			&attempt.State,
			&attempt.InputFileID,
		); err != nil {
			return nil, err
		}
		out = append(out, attempt)
	}

	return out, rows.Err()
}

// clearBatchAttemptInputFile journals one confirmed provider deletion. Only a
// deletion the provider acknowledged (including 404, which DeleteFile already
// maps to success) may clear the id.
//
// The id is set to NULL, not the empty string: input_file_id carries a UNIQUE
// constraint, so only one row could ever hold ”, and the upload path keys
// "never uploaded" off IS NULL.
func clearBatchAttemptInputFile(
	ctx context.Context,
	pool *pgxpool.Pool,
	attemptID int64,
) error {
	_, err := pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET input_file_id=NULL, updated_at=now()
WHERE id=$1`, attemptID)

	return err
}

func listFinalizingRuns(ctx context.Context, pool *pgxpool.Pool, limit int) ([]batchRun, error) {
	rows, err := pool.Query(ctx, `
SELECT id, state, model, endpoint, prompt_version, system_prompt,
       max_completion_tokens, reasoning_effort, completion_window,
       min_age_seconds, min_confidence, max_junk_rate, enable_purge,
       quarantine_days, failure_cooldown_seconds, item_count, created_at
FROM junkpurge_batch_runs
WHERE state='finalizing'
ORDER BY created_at
LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []batchRun
	for rows.Next() {
		var run batchRun
		var minAgeSeconds, cooldownSeconds int64
		if err := rows.Scan(
			&run.ID, &run.State, &run.Model, &run.Endpoint,
			&run.PromptVersion, &run.SystemPrompt, &run.MaxTokens,
			&run.ReasoningEffort, &run.CompletionWindow, &minAgeSeconds,
			&run.MinConfidence, &run.MaxJunkRate, &run.EnablePurge,
			&run.QuarantineDays, &cooldownSeconds, &run.ItemCount, &run.CreatedAt,
		); err != nil {
			return nil, err
		}
		run.MinAge = time.Duration(minAgeSeconds) * time.Second
		run.FailureCooldown = time.Duration(cooldownSeconds) * time.Second
		out = append(out, run)
	}
	return out, rows.Err()
}

func finalizeBatchRun(
	ctx context.Context,
	pool *pgxpool.Pool,
	run batchRun,
	currentPurgeEnabled bool,
) (batchFinalizeSummary, error) {
	summary := batchFinalizeSummary{
		RunID:          run.ID,
		QuarantineDays: run.QuarantineDays,
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return summary, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	if err := tx.QueryRow(ctx,
		`SELECT state FROM junkpurge_batch_runs WHERE id=$1 FOR UPDATE`,
		run.ID,
	).Scan(&state); err != nil {
		return summary, err
	}
	if state != runStateFinalizing {
		return summary, nil
	}

	// These evidence tables deliberately have no FK to torrents, so a torrent
	// row lock alone cannot stop a concurrent evidence insert between the
	// eligibility check and quarantine delete. SHARE blocks their writers for
	// this short settlement transaction and closes that destructive race.
	if _, err := tx.Exec(ctx, `
LOCK TABLE label_evidence, torrent_canonical_labels IN SHARE MODE`); err != nil {
		return summary, fmt.Errorf("junkpurge batch: lock evidence tables: %w", err)
	}

	rows, err := tx.Query(ctx, `
SELECT info_hash, torrent_name, verdict, confidence,
       input_tokens, cached_input_tokens, cache_write_tokens,
       output_tokens, reasoning_tokens
FROM junkpurge_batch_items
WHERE run_id=$1 AND state='succeeded'
ORDER BY ordinal
FOR UPDATE`, run.ID)
	if err != nil {
		return summary, err
	}
	type settledItem struct {
		hash []byte
		name string
		j    Judgment
	}
	var items []settledItem
	for rows.Next() {
		var item settledItem
		var usage tokenUsage
		if err := rows.Scan(
			&item.hash, &item.name, &item.j.Verdict, &item.j.Confidence,
			&usage.Input, &usage.Cached, &usage.CacheWrite,
			&usage.Output, &usage.Reasoning,
		); err != nil {
			rows.Close()
			return summary, err
		}
		items = append(items, item)
		summary.Usage.Input += usage.Input
		summary.Usage.Cached += usage.Cached
		summary.Usage.CacheWrite += usage.CacheWrite
		summary.Usage.Output += usage.Output
		summary.Usage.Reasoning += usage.Reasoning
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return summary, err
	}
	if len(items) != run.ItemCount {
		return summary, fmt.Errorf(
			"junkpurge batch: run %d finalizing with %d/%d successful items",
			run.ID, len(items), run.ItemCount)
	}

	if err := lockBatchTorrentsTx(ctx, tx, run.ID); err != nil {
		return summary, err
	}
	if err := lockBatchTorrentContentsTx(ctx, tx, run.ID); err != nil {
		return summary, err
	}
	current, err := currentBatchCandidatesTx(ctx, tx, run.ID, run.MinAge)
	if err != nil {
		return summary, err
	}
	var junk [][]byte
	for _, item := range items {
		summary.Judged++
		isJunk := item.j.IsJunk(run.MinConfidence)
		if isJunk {
			summary.Junk++
		}
		if _, ok := current[hex.EncodeToString(item.hash)]; !ok {
			continue
		}
		recorded, err := recordBatchJudgmentTx(
			ctx, tx, item.hash, item.j, item.name, run.CreatedAt,
		)
		if err != nil {
			return summary, err
		}
		if recorded && isJunk {
			junk = append(junk, item.hash)
		}
	}

	finalState := runStateCompleted
	if summary.Judged > 0 &&
		float64(summary.Junk)/float64(summary.Judged) > run.MaxJunkRate {
		finalState = runStateBreakerBlocked
	} else if !run.EnablePurge || !currentPurgeEnabled {
		finalState = runStateDryRun
		summary.WouldDelete = len(junk)
	} else if len(junk) > 0 {
		eligible, err := eligibleBatchJunkTx(
			ctx, tx, run.ID, junk, run.MinAge, run.MinConfidence,
		)
		if err != nil {
			return summary, err
		}
		summary.QuarantinedHashes, err = quarantineJunkTx(
			ctx, tx, eligible, run.MinConfidence,
		)
		if err != nil {
			return summary, err
		}
	}

	if _, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_items
SET state='applied', updated_at=now()
WHERE run_id=$1 AND state='succeeded'`, run.ID); err != nil {
		return summary, err
	}
	tag, err := tx.Exec(ctx, `
UPDATE junkpurge_batch_runs
SET state=$2, finalized_at=now(), updated_at=now()
WHERE id=$1 AND state='finalizing'`, run.ID, finalState)
	if err != nil {
		return summary, err
	}
	if tag.RowsAffected() != 1 {
		return summary, errors.New("junkpurge batch: finalization lost run lock")
	}
	if err := tx.Commit(ctx); err != nil {
		return summary, err
	}
	summary.State = finalState
	return summary, nil
}

func eligibleBatchJunkTx(
	ctx context.Context,
	tx pgx.Tx,
	runID int64,
	recordedJunk [][]byte,
	minAge time.Duration,
	minConfidence float64,
) ([][]byte, error) {
	if len(recordedJunk) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
SELECT t.info_hash
FROM junkpurge_batch_items i
JOIN torrents t ON t.info_hash=i.info_hash
WHERE i.run_id=$1
  AND t.private = false
  AND i.info_hash = ANY($4)
  AND i.state='succeeded'
  AND i.verdict='junk'
  AND i.confidence >= $2
  AND t.name=i.torrent_name
  AND EXISTS (
    SELECT 1 FROM torrent_contents tc
    WHERE tc.info_hash=t.info_hash
      AND tc.content_type IN ('movie','tv_show')
      AND tc.content_id IS NULL
      AND tc.created_at < now() - make_interval(secs => $3)
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_contents m
    WHERE m.info_hash=t.info_hash AND m.content_id IS NOT NULL
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_canonical_labels l WHERE l.info_hash=t.info_hash
  )
  AND NOT EXISTS (
    SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash
  )
ORDER BY i.ordinal`,
		runID, minConfidence, int64(minAge/time.Second), recordedJunk)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hashes [][]byte
	for rows.Next() {
		var hash []byte
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		hashes = append(hashes, hash)
	}
	return hashes, rows.Err()
}

func currentBatchCandidatesTx(
	ctx context.Context,
	tx pgx.Tx,
	runID int64,
	minAge time.Duration,
) (map[string]struct{}, error) {
	rows, err := tx.Query(ctx, `
SELECT i.info_hash
FROM junkpurge_batch_items i
JOIN torrents t ON t.info_hash=i.info_hash
WHERE i.run_id=$1
  AND t.private = false
  AND i.state='succeeded'
  AND t.name=i.torrent_name
  AND EXISTS (
    SELECT 1 FROM torrent_contents tc
    WHERE tc.info_hash=t.info_hash
      AND tc.content_type IN ('movie','tv_show')
      AND tc.content_id IS NULL
      AND tc.created_at < now() - make_interval(secs => $2)
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_contents m
    WHERE m.info_hash=t.info_hash AND m.content_id IS NOT NULL
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrent_canonical_labels l WHERE l.info_hash=t.info_hash
  )
  AND NOT EXISTS (
    SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash
  )`,
		runID, int64(minAge/time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var hash []byte
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out[hex.EncodeToString(hash)] = struct{}{}
	}
	return out, rows.Err()
}

func lockBatchTorrentsTx(
	ctx context.Context,
	tx pgx.Tx,
	runID int64,
) error {
	rows, err := tx.Query(ctx, `
SELECT t.info_hash
FROM torrents t
JOIN junkpurge_batch_items i ON i.info_hash=t.info_hash
WHERE i.run_id=$1 AND i.state='succeeded'
FOR UPDATE OF t`, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ignored []byte
		if err := rows.Scan(&ignored); err != nil {
			return err
		}
	}
	return rows.Err()
}

func lockBatchTorrentContentsTx(
	ctx context.Context,
	tx pgx.Tx,
	runID int64,
) error {
	rows, err := tx.Query(ctx, `
SELECT tc.info_hash
FROM torrent_contents tc
JOIN junkpurge_batch_items i ON i.info_hash=tc.info_hash
WHERE i.run_id=$1
FOR UPDATE OF tc`, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ignored []byte
		if err := rows.Scan(&ignored); err != nil {
			return err
		}
	}
	return rows.Err()
}

func recordBatchJudgmentTx(
	ctx context.Context,
	tx pgx.Tx,
	infoHash []byte,
	j Judgment,
	name string,
	runCreatedAt time.Time,
) (bool, error) {
	var recorded bool
	err := tx.QueryRow(ctx, `
INSERT INTO junkpurge_judgments (
  info_hash, verdict, confidence, reason, torrent_name, judged_at, purged
) VALUES ($1,$2,$3,$6,$4,now(),false)
ON CONFLICT (info_hash) DO UPDATE SET
  verdict=excluded.verdict,
  confidence=excluded.confidence,
  reason=excluded.reason,
  torrent_name=excluded.torrent_name,
  judged_at=excluded.judged_at,
  purged=false
WHERE junkpurge_judgments.judged_at <= $5
RETURNING true`,
		infoHash, j.Verdict, j.Confidence, name, runCreatedAt,
		judgmentReasonBatchRun,
	).Scan(&recorded)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return recorded, err
}
