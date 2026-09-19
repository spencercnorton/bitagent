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

const MaxResultBodyBytes = 128 << 10

type HTTPResult struct {
	Body       []byte
	StatusCode int
	ErrorClass string
}

// MatchDecision contains policy facts only. Source identity, parser evidence,
// candidates and model output come from the linked, privacy-admitted capture
// and result, not duplicated release names or raw info hashes.
type MatchDecision struct {
	Outcome            string  `json:"outcome"`
	GateReason         string  `json:"gate_reason"`
	ChosenID           int64   `json:"chosen_id"`
	Confidence         float64 `json:"confidence"`
	IsTV               bool    `json:"is_tv"`
	ResolvedYear       int     `json:"resolved_year"`
	MinConfidence      float64 `json:"min_confidence"`
	RequireSourceTitle bool    `json:"require_source_title"`
	WouldAttach        bool    `json:"would_attach"`
	Live               bool    `json:"live"`
}

// ResultRecorder is required when matcher capture is enabled. Production
// Recorder implements it; a configured recorder fails closed on storage errors.
type ResultRecorder interface {
	RecordHTTPResult(context.Context, []byte, HTTPResult) (ResultReceipt, error)
	RecordMatchDecision(context.Context, ResultReceipt, []byte, MatchDecision) error
}

type storedHTTPResult struct {
	CaptureKey     []byte
	Body           []byte
	ResponseSHA256 []byte
	StatusCode     int
	ErrorClass     string
	ObservedAt     time.Time
}

type resultStore interface {
	PersistHTTPResult(context.Context, storedHTTPResult) (bool, error)
	PersistMatchDecision(context.Context, ResultReceipt, []byte, []byte, time.Time) error
}

func (r *Recorder) RecordHTTPResult(ctx context.Context, captureKey []byte, result HTTPResult) (ResultReceipt, error) {
	if !r.Enabled() {
		return ResultReceipt{}, nil
	}
	if len(captureKey) != sha256.Size || len(result.Body) > MaxResultBodyBytes ||
		result.StatusCode < 0 || result.StatusCode > 599 {
		return ResultReceipt{}, fmt.Errorf("%w: malformed bounded HTTP result", ErrCaptureUnavailable)
	}
	switch result.ErrorClass {
	case "none", "transport", "read", "http_status", "envelope", "empty_choices", "audit_incomplete":
	default:
		return ResultReceipt{}, fmt.Errorf("%w: unknown HTTP result class", ErrCaptureUnavailable)
	}
	store, ok := r.store.(resultStore)
	if !ok {
		return ResultReceipt{}, fmt.Errorf("%w: result store is required", ErrCaptureUnavailable)
	}
	digest := sha256.Sum256(result.Body)
	recorded, err := store.PersistHTTPResult(ctx, storedHTTPResult{
		CaptureKey: append([]byte(nil), captureKey...), Body: append([]byte{}, result.Body...),
		ResponseSHA256: digest[:], StatusCode: result.StatusCode,
		ErrorClass: result.ErrorClass, ObservedAt: r.now().UTC(),
	})
	if err != nil {
		return ResultReceipt{}, fmt.Errorf("%w: persist HTTP result: %v", ErrCaptureUnavailable, err)
	}
	return ResultReceipt{
		CaptureKey: append([]byte(nil), captureKey...), ResponseSHA256: digest[:],
		FirstObservation: recorded, StatusCode: result.StatusCode, ErrorClass: result.ErrorClass,
	}, nil
}

func (r *Recorder) RecordMatchDecision(ctx context.Context, receipt ResultReceipt, infoHash []byte, decision MatchDecision) error {
	if !r.Enabled() {
		return nil
	}
	if len(receipt.CaptureKey) != sha256.Size ||
		len(receipt.ResponseSHA256) != sha256.Size || len(infoHash) != 20 {
		return fmt.Errorf("%w: decision requires a bound result receipt", ErrCaptureUnavailable)
	}
	if decision.Outcome == "" || len(decision.Outcome) > 64 || len(decision.GateReason) > 64 ||
		strings.ContainsAny(decision.Outcome+decision.GateReason, "\x00\r\n") ||
		math.IsNaN(decision.Confidence) || math.IsInf(decision.Confidence, 0) ||
		decision.Confidence < 0 || decision.Confidence > 1 ||
		math.IsNaN(decision.MinConfidence) || math.IsInf(decision.MinConfidence, 0) ||
		decision.MinConfidence <= 0 || decision.MinConfidence > 1 ||
		decision.ChosenID < 0 || decision.ResolvedYear < 0 ||
		(decision.WouldAttach && (decision.ChosenID == 0 || decision.GateReason != "" ||
			decision.Confidence < decision.MinConfidence || decision.Outcome != "matched")) {
		return fmt.Errorf("%w: malformed matcher decision", ErrCaptureUnavailable)
	}
	body, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("%w: marshal decision: %v", ErrCaptureUnavailable, err)
	}
	store, ok := r.store.(resultStore)
	if !ok {
		return fmt.Errorf("%w: result store is required", ErrCaptureUnavailable)
	}
	if err := store.PersistMatchDecision(ctx, receipt, infoHash, body, r.now().UTC()); err != nil {
		return fmt.Errorf("%w: persist matcher decision: %v", ErrCaptureUnavailable, err)
	}
	return nil
}

// KeyForRequest reproduces the recorder's content-addressed key after Capture
// has admitted the request. It confers no authority by itself: result writes
// require the existing unexpired capture and repeat native/qB privacy admission.
func KeyForRequest(req Request) ([]byte, error) {
	modelInput, err := canonicalJSONObject(req.ModelInputJSON)
	if err != nil {
		return nil, err
	}
	taskInput, err := canonicalJSONObject(req.TaskInputJSON)
	if err != nil {
		return nil, err
	}
	origin, err := normalizedSamplingOrigin(req.Task, req.SamplingOrigin)
	if err != nil {
		return nil, err
	}
	build := strings.TrimSpace(req.BuildIdentity)
	if build == "" {
		build = CurrentBuildIdentity()
	}
	source := namespacedDigest("bitagent-llm-evaluation-source-v1", req.InfoHash)
	group := namespacedDigest("bitagent-llm-evaluation-group-v1", req.GroupKey)
	input := digestParts(modelInput, taskInput)
	prompt := sha256.Sum256([]byte(req.SystemPrompt))
	endpoint := sha256.Sum256([]byte(req.Endpoint))
	contract := digestParts([]byte(build), []byte(req.ContractID), []byte(req.Model),
		[]byte(req.PromptVersion), prompt[:], endpoint[:])
	key := digestParts([]byte(req.Task), []byte(req.CandidateSource), []byte(origin),
		source[:], group[:], input[:], contract[:])
	return key[:], nil
}
