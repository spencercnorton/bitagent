package llmcapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// JunkDecision is the model/policy decision for one position in an actual HTTP
// request. WouldQuarantine is counterfactual eligibility, never application or
// independent review. The worker's outage and rate breakers still apply.
type JunkDecision struct {
	Outcome         string  `json:"outcome"`
	Verdict         string  `json:"verdict"`
	Confidence      float64 `json:"confidence"`
	MinConfidence   float64 `json:"min_confidence"`
	WouldQuarantine bool    `json:"would_quarantine"`
	Live            bool    `json:"live"`
	RequestIndex    int     `json:"request_index"`
	RequestSize     int     `json:"request_size"`
}

// JunkResultRecorder admits and retains whole grouped requests atomically. A
// per-source recorder cannot safely retain a grouped body containing a second
// source that fails privacy admission.
type JunkResultRecorder interface {
	CaptureJunkGroup(context.Context, []Request) error
	RecheckJunkGroup(context.Context, [][]byte) error
	RecordJunkHTTPResult(context.Context, [][]byte, HTTPResult) ([]ResultReceipt, error)
	RecordJunkDecisions(context.Context, []ResultReceipt, [][]byte, []JunkDecision) error
}

type junkGroupStore interface {
	PersistJunkGroup(context.Context, []storedCapture, [][]byte, int) error
	RecheckJunkGroup(context.Context, [][]byte) error
	PersistJunkHTTPResults(context.Context, []storedHTTPResult) error
	PersistJunkDecisions(context.Context, []ResultReceipt, [][]byte, [][]byte, time.Time) error
}

// prepareJunkStore lets Capture reuse its canonicalization, privacy and hash
// contract without publishing any text before the complete group is admitted.
type prepareJunkStore struct {
	records []storedCapture
	hashes  [][]byte
}

func (s *prepareJunkStore) PersistPublic(_ context.Context, c storedCapture, hash []byte, _ int) (persistOutcome, error) {
	s.records = append(s.records, c)
	s.hashes = append(s.hashes, append([]byte(nil), hash...))
	return persistRecorded, nil
}

func (r *Recorder) junkStore() (junkGroupStore, error) {
	if !r.Enabled() {
		return nil, fmt.Errorf("%w: enabled junk capture is required", ErrCaptureUnavailable)
	}
	s, ok := r.store.(junkGroupStore)
	if !ok {
		return nil, fmt.Errorf("%w: grouped junk result store is required", ErrCaptureUnavailable)
	}
	return s, nil
}

func (r *Recorder) CaptureJunkGroup(ctx context.Context, requests []Request) error {
	s, err := r.junkStore()
	if err != nil {
		return err
	}
	if len(requests) < 1 || len(requests) > 50 {
		return fmt.Errorf("%w: invalid junk request group size", ErrCaptureUnavailable)
	}
	buffer := &prepareJunkStore{}
	prepared := *r
	prepared.store = buffer
	at := r.now().UTC()
	prepared.now = func() time.Time { return at }
	seen := map[string]bool{}
	for i, req := range requests {
		if req.Task != TaskJunkPurge || req.SamplingOrigin == SamplingOriginSafetyTopUpCapture || seen[string(req.InfoHash)] {
			return fmt.Errorf("%w: invalid or duplicate junk request source", ErrCaptureUnavailable)
		}
		seen[string(req.InfoHash)] = true
		var input struct {
			Index int `json:"request_index"`
			Size  int `json:"request_size"`
		}
		if err := json.Unmarshal(req.TaskInputJSON, &input); err != nil || input.Index != i+1 || input.Size != len(requests) {
			return fmt.Errorf("%w: junk request position mismatch", ErrCaptureUnavailable)
		}
		if _, err := prepared.Capture(ctx, req); err != nil {
			return err
		}
		if i > 0 && (!bytes.Equal(buffer.records[0].ModelInputJSON, buffer.records[i].ModelInputJSON) ||
			!bytes.Equal(buffer.records[0].ContractSHA256, buffer.records[i].ContractSHA256)) {
			return fmt.Errorf("%w: junk group must share one exact HTTP contract", ErrCaptureUnavailable)
		}
	}
	if err := s.PersistJunkGroup(ctx, buffer.records, buffer.hashes, r.cfg.MaxRows); err != nil {
		return fmt.Errorf("%w: persist junk group: %w", ErrCaptureUnavailable, err)
	}
	return nil
}

func (r *Recorder) RecheckJunkGroup(ctx context.Context, keys [][]byte) error {
	s, err := r.junkStore()
	if err != nil {
		return err
	}
	if err := validJunkKeys(keys); err != nil {
		return err
	}
	if err := s.RecheckJunkGroup(ctx, keys); err != nil {
		return fmt.Errorf("%w: recheck junk group: %w", ErrCaptureUnavailable, err)
	}
	return nil
}

func validJunkKeys(keys [][]byte) error {
	if len(keys) < 1 || len(keys) > 50 {
		return fmt.Errorf("%w: invalid junk result group", ErrCaptureUnavailable)
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if len(key) != sha256.Size || seen[string(key)] {
			return fmt.Errorf("%w: invalid or duplicate junk capture key", ErrCaptureUnavailable)
		}
		seen[string(key)] = true
	}
	return nil
}

func (r *Recorder) RecordJunkHTTPResult(ctx context.Context, keys [][]byte, result HTTPResult) ([]ResultReceipt, error) {
	s, err := r.junkStore()
	if err != nil {
		return nil, err
	}
	if err := validJunkKeys(keys); err != nil {
		return nil, err
	}
	if len(result.Body) > MaxResultBodyBytes || result.StatusCode < 0 || result.StatusCode > 599 {
		return nil, fmt.Errorf("%w: invalid bounded junk result", ErrCaptureUnavailable)
	}
	switch result.ErrorClass {
	case "none", "transport", "read", "http_status":
	default:
		return nil, fmt.Errorf("%w: invalid junk result class", ErrCaptureUnavailable)
	}
	digest := sha256.Sum256(result.Body)
	stored := make([]storedHTTPResult, len(keys))
	receipts := make([]ResultReceipt, len(keys))
	at := r.now().UTC()
	for i, key := range keys {
		stored[i] = storedHTTPResult{CaptureKey: key, Body: result.Body, ResponseSHA256: digest[:], StatusCode: result.StatusCode, ErrorClass: result.ErrorClass, ObservedAt: at}
		receipts[i] = ResultReceipt{CaptureKey: append([]byte(nil), key...), ResponseSHA256: append([]byte(nil), digest[:]...), FirstObservation: true, StatusCode: result.StatusCode, ErrorClass: result.ErrorClass}
	}
	if err := s.PersistJunkHTTPResults(ctx, stored); err != nil {
		return nil, fmt.Errorf("%w: persist junk results: %w", ErrCaptureUnavailable, err)
	}
	return receipts, nil
}

func (r *Recorder) RecordJunkDecisions(ctx context.Context, receipts []ResultReceipt, hashes [][]byte, decisions []JunkDecision) error {
	s, err := r.junkStore()
	if err != nil {
		return err
	}
	if len(receipts) < 1 || len(receipts) > 50 || len(hashes) != len(receipts) || len(decisions) != len(receipts) {
		return fmt.Errorf("%w: junk decision group mismatch", ErrCaptureUnavailable)
	}
	bodies := make([][]byte, len(receipts))
	for i, d := range decisions {
		receipt := receipts[i]
		if len(receipt.CaptureKey) != 32 || len(receipt.ResponseSHA256) != 32 || len(hashes[i]) != 20 || d.RequestIndex != i+1 || d.RequestSize != len(receipts) || !validJunkDecision(d) {
			return fmt.Errorf("%w: invalid bound junk decision", ErrCaptureUnavailable)
		}
		if d.Outcome == "judged" && (receipt.StatusCode != 200 || receipt.ErrorClass != "none") {
			return fmt.Errorf("%w: junk judgment needs successful HTTP result", ErrCaptureUnavailable)
		}
		bodies[i], err = json.Marshal(d)
		if err != nil {
			return err
		}
	}
	if err := s.PersistJunkDecisions(ctx, receipts, hashes, bodies, r.now().UTC()); err != nil {
		return fmt.Errorf("%w: persist junk decisions: %w", ErrCaptureUnavailable, err)
	}
	return nil
}

func validJunkDecision(d JunkDecision) bool {
	if math.IsNaN(d.Confidence) || math.IsInf(d.Confidence, 0) || d.Confidence < 0 || d.Confidence > 1 || math.IsNaN(d.MinConfidence) || math.IsInf(d.MinConfidence, 0) || d.MinConfidence <= 0 || d.MinConfidence > 1 {
		return false
	}
	if d.Outcome == "invalid_response" {
		return d.Verdict == "" && d.Confidence == 0 && !d.WouldQuarantine
	}
	if d.Outcome != "judged" {
		return false
	}
	switch d.Verdict {
	case "junk", "real_mangled", "real_absent", "unsure":
	default:
		return false
	}
	return d.WouldQuarantine == (d.Verdict == "junk" && d.Confidence >= d.MinConfidence)
}
