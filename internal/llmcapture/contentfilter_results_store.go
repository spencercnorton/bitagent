package llmcapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *PostgresStore) FindContentFilterReplay(
	ctx context.Context,
	captureKey, infoHash []byte,
) (ContentFilterReplay, error) {
	if s == nil || s.pool == nil {
		return ContentFilterReplay{}, fmt.Errorf("contentfilter replay store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return ContentFilterReplay{}, err
	}
	var hasResult bool
	var responseSHA []byte
	var statusCode int
	var errorClass, decisionJSON string
	err = pool.QueryRow(ctx, `SELECT r.capture_key IS NOT NULL,
       COALESCE(r.response_sha256, decode('', 'hex')),
       COALESCE(r.http_status, 0), COALESCE(r.error_class, ''),
       COALESCE(r.decision::text, '')
FROM (`+resultPublicAdmissionSQL+`
  AND c.task = 'contentfilter' AND a.info_hash = $2
) admitted
LEFT JOIN llm_evaluation_capture_results r USING (capture_key)`, captureKey, infoHash).
		Scan(&hasResult, &responseSHA, &statusCode, &errorClass, &decisionJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContentFilterReplay{}, nil
	}
	if err != nil {
		return ContentFilterReplay{}, err
	}
	replay := ContentFilterReplay{Found: true}
	if hasResult {
		replay.Result = &ResultReceipt{
			CaptureKey: append([]byte(nil), captureKey...), ResponseSHA256: append([]byte(nil), responseSHA...),
			FirstObservation: true, StatusCode: statusCode, ErrorClass: errorClass,
		}
	}
	if decisionJSON == "" {
		return replay, nil
	}
	var decision ContentFilterDecision
	if err := json.Unmarshal([]byte(decisionJSON), &decision); err != nil {
		return ContentFilterReplay{}, fmt.Errorf("decode retained contentfilter decision: %w", err)
	}
	replay.Decision = &decision
	return replay, nil
}

func (s *PostgresStore) RecheckContentFilterRequest(ctx context.Context, captureKey, infoHash []byte) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("contentfilter request recheck store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	var admitted bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (`+resultPublicAdmissionSQL+`
  AND c.task = 'contentfilter' AND a.info_hash = $2
)`, captureKey, infoHash).Scan(&admitted)
	if err != nil {
		return err
	}
	if !admitted {
		return fmt.Errorf("contentfilter request is missing, mismatched, expired or no longer public")
	}
	return nil
}

func (s *PostgresStore) PersistContentFilterDecision(
	ctx context.Context,
	receipt ResultReceipt,
	infoHash, body []byte,
	at time.Time,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("contentfilter decision store has no database pool")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `WITH admitted AS (`+resultPublicAdmissionSQL+`
  AND a.info_hash = $3 AND c.task = 'contentfilter'
  AND c.task_input->'min_confidence' = $4::jsonb->'min_confidence'
  AND c.task_input->'live' = $4::jsonb->'live'
)
UPDATE llm_evaluation_capture_results r
SET decision = COALESCE(r.decision, $4::jsonb), decided_at = COALESCE(r.decided_at, $5)
WHERE r.capture_key = $1 AND r.response_sha256 = $2
  AND ((r.decision IS NULL AND $6) OR r.decision = $4::jsonb)
  AND (($4::jsonb->>'outcome' IN ('invalid_response', 'audit_incomplete'))
       OR (r.http_status = 200 AND r.error_class = 'none'))
  AND EXISTS (SELECT 1 FROM admitted)`,
		receipt.CaptureKey, receipt.ResponseSHA256, infoHash, body, at, receipt.FirstObservation,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("first contentfilter result is missing, already decided, mismatched, expired or no longer public")
	}
	return nil
}
