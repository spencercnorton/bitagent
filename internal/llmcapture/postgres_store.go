package llmcapture

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
)

type PostgresStore struct {
	pool lazy.Lazy[*pgxpool.Pool]
}

func NewPostgresStore(pool lazy.Lazy[*pgxpool.Pool]) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const captureAdmissionLockSQL = `
SELECT pg_advisory_xact_lock(
  hashtext('bitagent'), hashtext('llm_evaluation_capture')
)`

// DeleteExpired enforces time-based retention even when no future capture is
// written. Deleting the parent capture cascades to the local raw-infohash
// admission mapping in the same transaction.
func (s *PostgresStore) DeleteExpired(
	ctx context.Context,
) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("capture store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("acquire pool: %w", err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin expiry cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, captureAdmissionLockSQL); err != nil {
		return 0, fmt.Errorf("expiry cleanup lock: %w", err)
	}
	tag, err := tx.Exec(ctx, `
DELETE FROM llm_evaluation_captures
WHERE expires_at <= transaction_timestamp()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired captures: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit expiry cleanup: %w", err)
	}
	return tag.RowsAffected(), nil
}

// captureInsertSQL performs the final native-private and qB/bitgrab evidence
// recheck in the same database statement that admits the model-visible text.
// $22 (the raw info hash) is used only in WHERE predicates and the separate
// local admission mapping; it is never selected into the capture artifact.
const captureInsertSQL = `
WITH public_admission AS (
  SELECT 1
  FROM torrents t
  WHERE t.info_hash = $22
    AND t.private = false
    AND NOT EXISTS (
      SELECT 1
      FROM label_evidence e
      WHERE e.info_hash = $22
        AND e.source = 'qbittorrent'
        AND lower(e.category) IN ('private', 'bitgrab')
    )
),
inserted AS (
  INSERT INTO llm_evaluation_captures (
    capture_key,
    task,
    candidate_source,
    source_sha256,
    group_sha256,
    input_sha256,
    contract_sha256,
    prompt_sha256,
    endpoint_sha256,
    model,
    prompt_version,
    build_identity,
    contract_id,
    system_prompt,
    model_input,
    task_input,
    capture_schema_version,
    privacy_status,
    sampling_origin,
    captured_at,
    expires_at,
    privacy_checked_at
  )
  SELECT
    $1, $2, nullif($3, ''), $4, $5, $6, $7, $8, $9, $10,
    $11, $12, $13, $14, $15, $16, $17,
    'verified_native_public_qb_rechecked', $18, $19, $20, $21
  FROM public_admission
  ON CONFLICT (capture_key) DO NOTHING
  RETURNING 1
)
SELECT
  EXISTS (SELECT 1 FROM public_admission),
  EXISTS (SELECT 1 FROM inserted)`

const captureAdmissionIdentitySQL = `
INSERT INTO llm_evaluation_capture_admissions (
  capture_key,
  info_hash,
  expires_at
)
VALUES ($1, $2, $3)
ON CONFLICT (capture_key) DO NOTHING`

func (s *PostgresStore) PersistPublic(
	ctx context.Context,
	record storedCapture,
	rawInfoHash []byte,
	maxRows int,
) (persistOutcome, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("capture store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return 0, fmt.Errorf("acquire pool: %w", err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, captureAdmissionLockSQL); err != nil {
		return 0, fmt.Errorf("admission lock: %w", err)
	}
	if _, err := tx.Exec(
		ctx,
		`DELETE FROM llm_evaluation_captures WHERE expires_at <= $1`,
		record.CapturedAt,
	); err != nil {
		return 0, fmt.Errorf("expire: %w", err)
	}

	var public, inserted bool
	if err := tx.QueryRow(
		ctx,
		captureInsertSQL,
		record.CaptureKey,
		string(record.Task),
		string(record.CandidateSource),
		record.SourceSHA256,
		record.GroupSHA256,
		record.InputSHA256,
		record.ContractSHA256,
		record.PromptSHA256,
		record.EndpointSHA256,
		record.Model,
		record.PromptVersion,
		record.BuildIdentity,
		record.ContractID,
		record.SystemPrompt,
		record.ModelInputJSON,
		record.TaskInputJSON,
		CaptureSchemaVersion,
		record.SamplingOrigin,
		record.CapturedAt,
		record.ExpiresAt,
		record.PrivacyCheckedAt,
		rawInfoHash,
	).Scan(&public, &inserted); err != nil {
		return 0, fmt.Errorf("admit: %w", err)
	}
	if !public {
		return persistPrivacyBlocked, nil
	}
	if _, err := tx.Exec(
		ctx,
		captureAdmissionIdentitySQL,
		record.CaptureKey,
		rawInfoHash,
		record.ExpiresAt,
	); err != nil {
		return 0, fmt.Errorf("persist admission identity: %w", err)
	}
	var identityMatches bool
	if err := tx.QueryRow(
		ctx,
		`SELECT EXISTS (
		   SELECT 1
		   FROM llm_evaluation_capture_admissions
		   WHERE capture_key = $1 AND info_hash = $2
		 )`,
		record.CaptureKey,
		rawInfoHash,
	).Scan(&identityMatches); err != nil {
		return 0, fmt.Errorf("verify admission identity: %w", err)
	}
	if !identityMatches {
		return 0, fmt.Errorf("admission identity missing or mismatched")
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM llm_evaluation_captures
WHERE id IN (
  SELECT id
  FROM llm_evaluation_captures
  ORDER BY captured_at DESC, id DESC
  OFFSET $1
)`, maxRows); err != nil {
		return 0, fmt.Errorf("enforce row cap: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	if inserted {
		return persistRecorded, nil
	}
	return persistDuplicate, nil
}
