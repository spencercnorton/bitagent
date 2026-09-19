package llmcapture

import (
	"context"
	"fmt"
	"time"
)

// Recheck the live source at the write boundary, not just when the request was
// captured. A deletion, expiry or private/bitgrab reclassification fails closed.
const resultPublicAdmissionSQL = `
SELECT c.capture_key
FROM llm_evaluation_captures c
JOIN llm_evaluation_capture_admissions a USING (capture_key)
JOIN torrents t ON t.info_hash = a.info_hash
WHERE c.capture_key = $1
  AND c.task IN ('matcher_extract', 'matcher_rerank', 'classifier_type', 'contentfilter')
  AND c.expires_at > transaction_timestamp()
  AND a.expires_at = c.expires_at
  AND t.private = false
  AND NOT EXISTS (
    SELECT 1 FROM label_evidence e
    WHERE e.info_hash = a.info_hash AND e.source = 'qbittorrent'
      AND lower(e.category) IN ('private', 'bitgrab')
  )`

func (s *PostgresStore) PersistHTTPResult(ctx context.Context, result storedHTTPResult) (bool, error) {
	if s == nil || s.pool == nil {
		return false, fmt.Errorf("result store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return false, err
	}
	var public, inserted bool
	err = pool.QueryRow(ctx, `WITH admitted AS (`+resultPublicAdmissionSQL+`), inserted AS (
INSERT INTO llm_evaluation_capture_results (
  capture_key, response_body, response_sha256, http_status, error_class, observed_at
)
SELECT capture_key, $2, $3, $4, $5, $6 FROM admitted
ON CONFLICT (capture_key) DO NOTHING RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM admitted), EXISTS (SELECT 1 FROM inserted)`,
		result.CaptureKey, result.Body, result.ResponseSHA256,
		result.StatusCode, result.ErrorClass, result.ObservedAt,
	).Scan(&public, &inserted)
	if err != nil {
		return false, err
	}
	if !public {
		return false, fmt.Errorf("capture is absent, expired or no longer public")
	}
	return inserted, nil
}

func (s *PostgresStore) PersistMatchDecision(ctx context.Context, receipt ResultReceipt, infoHash, body []byte, at time.Time) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("result store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `WITH admitted AS (`+resultPublicAdmissionSQL+`
  AND a.info_hash = $3 AND c.task = 'matcher_rerank'
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
		return fmt.Errorf("first result is missing, already decided, mismatched, expired or no longer public")
	}
	return nil
}
