package llmcapture

import (
	"context"
	"fmt"
	"time"
)

func (s *PostgresStore) RecheckTypeRequest(ctx context.Context, captureKey, infoHash []byte) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("type request recheck store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	var admitted bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (`+resultPublicAdmissionSQL+`
  AND c.task = 'classifier_type' AND a.info_hash = $2
)`, captureKey, infoHash).Scan(&admitted)
	if err != nil {
		return err
	}
	if !admitted {
		return fmt.Errorf("type request is missing, mismatched, expired or no longer public")
	}
	return nil
}

// PersistTypeDecision repeats live privacy admission and binds both the source
// hash and immutable first response. Duplicate receipts may verify an identical
// existing decision, but cannot create or replace one.
func (s *PostgresStore) PersistTypeDecision(ctx context.Context, receipt ResultReceipt, infoHash, body []byte, at time.Time) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("type decision store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `WITH admitted AS (`+resultPublicAdmissionSQL+`
  AND a.info_hash = $3 AND c.task = 'classifier_type'
  AND c.task_input->'min_confidence' = $4::jsonb->'min_confidence'
  AND c.task_input->'live' = $4::jsonb->'live'
)
UPDATE llm_evaluation_capture_results r
SET decision = COALESCE(r.decision, $4::jsonb), decided_at = COALESCE(r.decided_at, $5)
WHERE r.capture_key = $1 AND r.response_sha256 = $2
  AND ((r.decision IS NULL AND $6) OR r.decision = $4::jsonb)
  AND r.http_status = 200 AND r.error_class = 'none'
  AND EXISTS (SELECT 1 FROM admitted)`,
		receipt.CaptureKey, receipt.ResponseSHA256, infoHash, body, at, receipt.FirstObservation,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("first type result is missing, already decided, mismatched, expired or no longer public")
	}
	return nil
}
