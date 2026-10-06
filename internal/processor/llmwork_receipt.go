package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
)

// deferredReceiptDecision proves current, source-bound retained response and
// immutable decision identity inside the eventual application transaction.
// A plausible confidence or a capture key by itself is not authority.
func deferredReceiptDecision(ctx context.Context, tx pgx.Tx, task llmwork.Task, receipt llmcapture.ResultReceipt, kind llmcapture.Task) (json.RawMessage, error) {
	if len(receipt.CaptureKey) != 32 || len(receipt.ResponseSHA256) != 32 || receipt.StatusCode != 200 || receipt.ErrorClass != "none" {
		return nil, llmwork.ErrObsolete
	}
	var digest, decision []byte
	var status int
	var errorClass string
	err := tx.QueryRow(ctx, `SELECT r.response_sha256,r.http_status,r.error_class,r.decision
FROM llm_evaluation_captures c
JOIN llm_evaluation_capture_admissions a USING(capture_key)
JOIN llm_evaluation_capture_results r USING(capture_key)
JOIN llm_capture_dispatch_attempts d ON COALESCE(d.active_capture_key,d.capture_key)=c.capture_key
WHERE c.capture_key=$1 AND a.info_hash=$2 AND c.task=$3
AND c.expires_at>clock_timestamp() AND a.expires_at>clock_timestamp()
AND c.privacy_status='verified_native_public_qb_rechecked'
AND d.task_key=$4 AND d.task=c.task AND d.state='result' AND d.response_sha256=r.response_sha256
AND d.response_observed_at=r.observed_at AND NOT d.legacy_unproven
AND r.decided_at IS NOT NULL FOR SHARE OF c,a,r,d`, receipt.CaptureKey, task.InfoHash, string(kind), task.Key).Scan(&digest, &status, &errorClass, &decision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, llmwork.ErrObsolete
	}
	if err != nil {
		return nil, err
	}
	if status != 200 || errorClass != "none" || !bytes.Equal(digest, receipt.ResponseSHA256) || len(decision) == 0 {
		return nil, llmwork.ErrObsolete
	}
	return decision, nil
}

func deferredTypeDecisionMatches(raw json.RawMessage, category string, confidence float64) bool {
	var d llmcapture.TypeDecision
	if json.Unmarshal(raw, &d) != nil {
		return false
	}
	return d.Outcome == "classified" && d.Live && d.WouldApply && d.Category == category && d.Confidence == confidence &&
		d.Confidence >= d.MinConfidence && llmcapture.TypeLiveAllowed(category, d.LiveAllowedTypes)
}
