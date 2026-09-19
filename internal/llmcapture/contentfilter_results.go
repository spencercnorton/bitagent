package llmcapture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// ContentFilterDecision contains policy facts only. The source title and raw
// provider output remain in the linked privacy-admitted capture and result.
type ContentFilterDecision struct {
	Outcome       string  `json:"outcome"`
	IsEnglish     bool    `json:"is_english"`
	Confidence    float64 `json:"confidence"`
	Reason        string  `json:"reason"`
	MinConfidence float64 `json:"min_confidence"`
	WouldDrop     bool    `json:"would_drop"`
	Live          bool    `json:"live"`
}

// ContentFilterResultRecorder is intentionally separate from the matcher and
// type-classifier contracts so one stage cannot write another stage's policy.
type ContentFilterResultRecorder interface {
	FindContentFilterReplay(context.Context, []byte, []byte) (ContentFilterReplay, error)
	RecheckContentFilterRequest(context.Context, []byte, []byte) error
	RecordHTTPResult(context.Context, []byte, HTTPResult) (ResultReceipt, error)
	RecordContentFilterDecision(context.Context, ResultReceipt, []byte, ContentFilterDecision) error
}

// ContentFilterReplay reports immutable evidence already retained for the
// exact content-addressed request. A nil Decision or Result is an interrupted
// audit chain that callers must complete locally without another paid dispatch.
type ContentFilterReplay struct {
	Found    bool
	Result   *ResultReceipt
	Decision *ContentFilterDecision
}

type contentFilterDecisionStore interface {
	PersistContentFilterDecision(context.Context, ResultReceipt, []byte, []byte, time.Time) error
}

type contentFilterRequestStore interface {
	FindContentFilterReplay(context.Context, []byte, []byte) (ContentFilterReplay, error)
	RecheckContentFilterRequest(context.Context, []byte, []byte) error
}

func (r *Recorder) FindContentFilterReplay(ctx context.Context, captureKey, infoHash []byte) (ContentFilterReplay, error) {
	if !r.Enabled() || len(captureKey) != sha256.Size || len(infoHash) != 20 {
		return ContentFilterReplay{}, fmt.Errorf("%w: enabled contentfilter replay requires bound source identity", ErrCaptureUnavailable)
	}
	store, ok := r.store.(contentFilterRequestStore)
	if !ok {
		return ContentFilterReplay{}, fmt.Errorf("%w: contentfilter replay store is required", ErrCaptureUnavailable)
	}
	replay, err := store.FindContentFilterReplay(ctx, captureKey, infoHash)
	if err != nil {
		return ContentFilterReplay{}, fmt.Errorf("%w: contentfilter replay lookup: %v", ErrCaptureUnavailable, err)
	}
	if replay.Decision != nil && !validContentFilterDecision(*replay.Decision) {
		return ContentFilterReplay{}, fmt.Errorf("%w: malformed retained contentfilter decision", ErrCaptureUnavailable)
	}
	if replay.Result != nil && (len(replay.Result.CaptureKey) != sha256.Size ||
		len(replay.Result.ResponseSHA256) != sha256.Size) {
		return ContentFilterReplay{}, fmt.Errorf("%w: malformed retained contentfilter result", ErrCaptureUnavailable)
	}
	if replay.Decision != nil && replay.Result == nil {
		return ContentFilterReplay{}, fmt.Errorf("%w: retained contentfilter decision has no result", ErrCaptureUnavailable)
	}
	return replay, nil
}

func (r *Recorder) RecheckContentFilterRequest(ctx context.Context, captureKey, infoHash []byte) error {
	if !r.Enabled() || len(captureKey) != sha256.Size || len(infoHash) != 20 {
		return fmt.Errorf("%w: enabled contentfilter request recheck requires bound source identity", ErrCaptureUnavailable)
	}
	store, ok := r.store.(contentFilterRequestStore)
	if !ok {
		return fmt.Errorf("%w: contentfilter request recheck store is required", ErrCaptureUnavailable)
	}
	if err := store.RecheckContentFilterRequest(ctx, captureKey, infoHash); err != nil {
		return fmt.Errorf("%w: contentfilter request admission recheck: %v", ErrCaptureUnavailable, err)
	}
	return nil
}

func (r *Recorder) RecordContentFilterDecision(
	ctx context.Context,
	receipt ResultReceipt,
	infoHash []byte,
	decision ContentFilterDecision,
) error {
	if !r.Enabled() {
		return nil
	}
	successfulResult := receipt.StatusCode == 200 && receipt.ErrorClass == "none"
	terminalOnly := decision.Outcome == "invalid_response" || decision.Outcome == "audit_incomplete"
	if len(receipt.CaptureKey) != sha256.Size || len(receipt.ResponseSHA256) != sha256.Size ||
		len(infoHash) != 20 || !validContentFilterDecision(decision) ||
		(!terminalOnly && !successfulResult) {
		return fmt.Errorf("%w: malformed or unbound contentfilter decision", ErrCaptureUnavailable)
	}
	body, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("%w: marshal contentfilter decision: %v", ErrCaptureUnavailable, err)
	}
	store, ok := r.store.(contentFilterDecisionStore)
	if !ok {
		return fmt.Errorf("%w: contentfilter decision store is required", ErrCaptureUnavailable)
	}
	if err := store.PersistContentFilterDecision(ctx, receipt, infoHash, body, r.now().UTC()); err != nil {
		return fmt.Errorf("%w: persist contentfilter decision: %v", ErrCaptureUnavailable, err)
	}
	return nil
}

func validContentFilterDecision(d ContentFilterDecision) bool {
	if math.IsNaN(d.Confidence) || math.IsInf(d.Confidence, 0) || d.Confidence < 0 || d.Confidence > 1 ||
		math.IsNaN(d.MinConfidence) || math.IsInf(d.MinConfidence, 0) || d.MinConfidence <= 0 || d.MinConfidence > 1 ||
		len(d.Reason) > 64 || strings.ContainsAny(d.Reason, "\x00\r\n") {
		return false
	}
	switch d.Outcome {
	case "english":
		return d.IsEnglish && d.Reason != "" && !d.WouldDrop
	case "non_english":
		return !d.IsEnglish && d.Reason != "" && d.Confidence >= d.MinConfidence && d.WouldDrop
	case "low_confidence":
		return !d.IsEnglish && d.Reason != "" && d.Confidence < d.MinConfidence && !d.WouldDrop
	case "invalid_response":
		return !d.IsEnglish && d.Confidence == 0 && d.Reason == "" && !d.WouldDrop
	case "audit_incomplete":
		return !d.IsEnglish && d.Confidence == 0 && d.Reason == "" && !d.WouldDrop
	default:
		return false
	}
}
