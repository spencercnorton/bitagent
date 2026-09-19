package llmcapture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// TypeDecision records only the type-only fallback's policy facts. Classified
// means the confidence/type gates passed; Live identifies whether application
// was enabled, not proof that a downstream database write completed.
// Provider/source text belongs solely to the linked admitted request/result.
type TypeDecision struct {
	Outcome       string  `json:"outcome"`
	Category      string  `json:"category"`
	Confidence    float64 `json:"confidence"`
	MinConfidence float64 `json:"min_confidence"`
	WouldApply    bool    `json:"would_apply"`
	Live          bool    `json:"live"`
}

// TypeResultRecorder is separate from ResultRecorder: adding a type-only
// consumer must not give it the matcher's TMDB attachment decision contract.
type TypeResultRecorder interface {
	RecheckTypeRequest(context.Context, []byte, []byte) error
	RecordHTTPResult(context.Context, []byte, HTTPResult) (ResultReceipt, error)
	RecordTypeDecision(context.Context, ResultReceipt, []byte, TypeDecision) error
}

type typeDecisionStore interface {
	PersistTypeDecision(context.Context, ResultReceipt, []byte, []byte, time.Time) error
}

type typeRequestStore interface {
	RecheckTypeRequest(context.Context, []byte, []byte) error
}

// RecheckTypeRequest is the final bounded admission check before dispatch. A
// content-addressed key alone confers no authority: the live source must still
// exist, match the exact local info hash, be public and retain its admission.
// This narrows the capture-to-egress window; it does not lock database writers
// across the subsequent HTTP request or claim to eliminate that race.
func (r *Recorder) RecheckTypeRequest(ctx context.Context, captureKey, infoHash []byte) error {
	if !r.Enabled() || len(captureKey) != sha256.Size || len(infoHash) != 20 {
		return fmt.Errorf("%w: enabled type request recheck requires bound source identity", ErrCaptureUnavailable)
	}
	store, ok := r.store.(typeRequestStore)
	if !ok {
		return fmt.Errorf("%w: type request recheck store is required", ErrCaptureUnavailable)
	}
	if err := store.RecheckTypeRequest(ctx, captureKey, infoHash); err != nil {
		return fmt.Errorf("%w: type request admission recheck: %v", ErrCaptureUnavailable, err)
	}
	return nil
}

func (r *Recorder) RecordTypeDecision(ctx context.Context, receipt ResultReceipt, infoHash []byte, decision TypeDecision) error {
	if !r.Enabled() {
		return nil
	}
	if len(receipt.CaptureKey) != sha256.Size || len(receipt.ResponseSHA256) != sha256.Size ||
		len(infoHash) != 20 || receipt.StatusCode != 200 || receipt.ErrorClass != "none" {
		return fmt.Errorf("%w: type decision requires a bound successful result receipt", ErrCaptureUnavailable)
	}
	if !validTypeDecision(decision) {
		return fmt.Errorf("%w: malformed classifier type decision", ErrCaptureUnavailable)
	}
	body, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("%w: marshal type decision: %v", ErrCaptureUnavailable, err)
	}
	store, ok := r.store.(typeDecisionStore)
	if !ok {
		return fmt.Errorf("%w: type decision store is required", ErrCaptureUnavailable)
	}
	if err := store.PersistTypeDecision(ctx, receipt, infoHash, body, r.now().UTC()); err != nil {
		return fmt.Errorf("%w: persist type decision: %v", ErrCaptureUnavailable, err)
	}
	return nil
}

func validTypeDecision(d TypeDecision) bool {
	if math.IsNaN(d.Confidence) || math.IsInf(d.Confidence, 0) || d.Confidence < 0 || d.Confidence > 1 ||
		math.IsNaN(d.MinConfidence) || math.IsInf(d.MinConfidence, 0) || d.MinConfidence <= 0 || d.MinConfidence > 1 {
		return false
	}
	known := false
	switch d.Category {
	case "movie", "tv", "music", "audiobook", "book":
		known = true
	case "unknown", "":
	default:
		return false
	}
	switch d.Outcome {
	case "classified":
		return known && d.Confidence >= d.MinConfidence && d.WouldApply
	case "unknown":
		return d.Category == "unknown" && !d.WouldApply
	case "low_confidence":
		return known && d.Confidence < d.MinConfidence && !d.WouldApply
	case "invalid_response":
		return d.Category == "" && d.Confidence == 0 && !d.WouldApply
	default:
		return false
	}
}
