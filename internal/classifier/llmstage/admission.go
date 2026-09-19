package llmstage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
)

// CallBudget reserves one possible billed dispatch durably. Failures are never
// refunded. Production uses the separate classifier_type PostgreSQL scope.
type CallBudget interface {
	Reserve(context.Context, int, int) (bool, error)
}

// Admission is mandatory for enabled stages, including shadow mode. There is
// deliberately no in-memory production fallback when durable storage fails.
type Admission struct {
	Budget  CallBudget
	Capture llmcapture.Capturer
}

func (s *Stage) admissionReady() error {
	if s.admission.Budget == nil || s.admission.Capture == nil || !s.admission.Capture.Enabled() {
		s.metrics.gateRejectsTotal.WithLabelValues("audit_unavailable").Inc()
		return llmcapture.ErrCaptureUnavailable
	}
	if _, ok := s.admission.Capture.(llmcapture.TypeResultRecorder); !ok {
		s.metrics.gateRejectsTotal.WithLabelValues("audit_unavailable").Inc()
		return llmcapture.ErrCaptureUnavailable
	}
	return nil
}

func (s *Stage) callOpenAI(ctx context.Context, t model.Torrent, body []byte) (Decision, error) {
	if err := s.cfg.Validate(); err != nil {
		return Decision{}, err
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if time.Now().UnixNano() < s.retryAfter.Load() {
		s.metrics.gateRejectsTotal.WithLabelValues("budget_cooldown").Inc()
		return Decision{}, errors.New("type call allowance cooldown")
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		s.metrics.gateRejectsTotal.WithLabelValues("concurrency").Inc()
		return Decision{}, errors.New("type concurrency allowance exhausted")
	}
	// Bound DB admission, privacy checks and HTTP together; response persistence
	// gets a separate short cleanup deadline if dispatch consumed this context.
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	ok, err := s.admission.Budget.Reserve(ctx, s.cfg.DailyCallLimit, s.cfg.MonthlyCallLimit)
	if err != nil || !ok {
		reason := "budget_exhausted"
		if err != nil {
			reason = "budget_unavailable"
		}
		s.metrics.gateRejectsTotal.WithLabelValues(reason).Inc()
		s.retryAfter.Store(time.Now().Add(time.Minute).UnixNano())
		return Decision{}, fmt.Errorf("type admission denied: %s", reason)
	}
	taskInputObject := map[string]any{
		"min_confidence": s.cfg.MinConfidence, "live": s.cfg.EnableLive,
	}
	if s.cfg.OpenrouterProvider != "" {
		taskInputObject["openrouter_provider"] = s.cfg.OpenrouterProvider
	}
	if s.cfg.OpenaiDataSharing {
		taskInputObject["openai_data_sharing"] = true
	}
	taskInput, err := json.Marshal(taskInputObject)
	if err != nil {
		return Decision{}, err
	}
	capture := llmcapture.Request{
		Task: llmcapture.TaskClassifierType, InfoHash: t.InfoHash.Bytes(),
		GroupKey: []byte(contentfilter.EvaluationGroupKey(t.Name)), NativePrivate: t.Private,
		Model: s.cfg.Model, Endpoint: s.cfg.Endpoint, PromptVersion: s.cfg.PromptVersion,
		SystemPrompt: systemPrompt, ModelInputJSON: body, TaskInputJSON: taskInput,
		BuildIdentity: llmcapture.CurrentBuildIdentity(), ContractID: typeContractID(s.cfg),
	}
	outcome, err := s.admission.Capture.Capture(ctx, capture)
	if err != nil || (outcome != llmcapture.OutcomeRecorded && outcome != llmcapture.OutcomeDuplicate) {
		s.metrics.gateRejectsTotal.WithLabelValues("audit_unavailable").Inc()
		return Decision{}, llmcapture.ErrCaptureUnavailable
	}
	// Only the first response is retained. A cold cache or failed attempt must
	// not buy an unrecordable second response for the same source/request/policy.
	if outcome == llmcapture.OutcomeDuplicate {
		s.metrics.gateRejectsTotal.WithLabelValues("already_captured").Inc()
		return Decision{}, llmcapture.ErrCaptureUnavailable
	}
	key, err := llmcapture.KeyForRequest(capture)
	if err != nil {
		return Decision{}, err
	}
	// Repeat evidence privacy at the final outbound boundary, after potentially
	// slow admission writes. Native privacy is also checked by Capture's SQL.
	private, err := s.privacy.IsPrivateInfoHash(ctx, t.InfoHash.Bytes())
	if err != nil || private || t.Private {
		s.metrics.gateRejectsTotal.WithLabelValues("privacy").Inc()
		return Decision{}, llmcapture.ErrPrivacyBlocked
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if err := s.admission.Capture.(llmcapture.TypeResultRecorder).RecheckTypeRequest(ctx, key, t.InfoHash.Bytes()); err != nil {
		s.metrics.gateRejectsTotal.WithLabelValues("privacy").Inc()
		return Decision{}, llmcapture.ErrCaptureUnavailable
	}
	s.metrics.callsTotal.WithLabelValues(s.cfg.Model).Inc()
	started := time.Now()
	resp, requestErr := s.http.Do(req)
	result := llmcapture.HTTPResult{ErrorClass: "transport"}
	if requestErr == nil {
		defer resp.Body.Close()
		const maxResponse = 64 << 10
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		result.StatusCode, result.Body, result.ErrorClass = resp.StatusCode, raw, "none"
		if len(raw) > maxResponse {
			result.Body = raw[:maxResponse]
			readErr = errors.New("type response exceeds byte limit")
		}
		if readErr != nil {
			result.ErrorClass, requestErr = "read", readErr
		} else if resp.StatusCode != http.StatusOK {
			result.ErrorClass, requestErr = "http_status", fmt.Errorf("type provider HTTP %d", resp.StatusCode)
		}
	}
	s.metrics.callDuration.Observe(time.Since(started).Seconds())
	// Usage is an efficiency signal for every admitted dispatch, including
	// provider errors. Record it before the audit write so a storage outage
	// cannot make provider spend disappear from operational metrics.
	s.recordUsage(result.Body)
	// A response remains evidence even if cancellation arrived during the call.
	// Never retry a paid request just because this local write failed.
	cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	receipt, recordErr := s.admission.Capture.(llmcapture.TypeResultRecorder).RecordHTTPResult(cleanup, key, result)
	cleanupCancel()
	if recordErr != nil {
		s.metrics.auditTotal.WithLabelValues("result_error").Inc()
		return Decision{}, llmcapture.ErrCaptureUnavailable
	}
	s.metrics.auditTotal.WithLabelValues("result_recorded").Inc()
	if requestErr != nil {
		s.metrics.callErrorsTotal.WithLabelValues(result.ErrorClass).Inc()
		return Decision{}, requestErr
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	decision, err := parseResponse(result.Body)
	decision.receipt = receipt
	if err != nil {
		s.metrics.callErrorsTotal.WithLabelValues("schema").Inc()
		if auditErr := s.recordDecision(ctx, t, decision, true); auditErr != nil {
			return Decision{}, auditErr
		}
		return Decision{}, err
	}
	return decision, nil
}

func typeContractID(cfg Config) string {
	if cfg.OpenaiDataSharing {
		return "classifier-type-v3-openai-data-sharing"
	}
	if cfg.OpenrouterProvider != "" {
		return "classifier-type-v2-openrouter"
	}
	return "classifier-type-v1"
}

func (s *Stage) recordUsage(raw []byte) {
	var response chatUsageResponse
	if json.Unmarshal(raw, &response) != nil || response.Usage == nil {
		s.metrics.usageMissingTotal.WithLabelValues(s.cfg.Model).Inc()
		return
	}
	u := response.Usage
	if u.PromptTokens == nil || u.CompletionTokens == nil || *u.PromptTokens < 0 || *u.CompletionTokens < 0 ||
		u.PromptDetails.CachedTokens < 0 || u.PromptDetails.CachedTokens > *u.PromptTokens ||
		u.CompletionDetails.ReasoningTokens < 0 || u.CompletionDetails.ReasoningTokens > *u.CompletionTokens {
		s.metrics.usageMissingTotal.WithLabelValues(s.cfg.Model).Inc()
		return
	}
	for kind, n := range map[string]int64{
		"input": *u.PromptTokens, "cached_input": u.PromptDetails.CachedTokens,
		"output": *u.CompletionTokens, "reasoning": u.CompletionDetails.ReasoningTokens,
	} {
		s.metrics.tokensTotal.WithLabelValues(s.cfg.Model, kind).Add(float64(n))
	}
}

func (s *Stage) recordDecision(ctx context.Context, t model.Torrent, d Decision, invalid bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	policy := llmcapture.TypeDecision{
		Category: string(d.MediaType), Confidence: d.Confidence,
		MinConfidence: s.cfg.MinConfidence, Live: s.cfg.EnableLive,
	}
	_, known := mediaTypeToContentType(d.MediaType)
	switch {
	case invalid:
		policy.Outcome, policy.Category, policy.Confidence = "invalid_response", "", 0
	case !known:
		policy.Outcome = "unknown"
	case d.Confidence < s.cfg.MinConfidence:
		policy.Outcome = "low_confidence"
	default:
		policy.Outcome, policy.WouldApply = "classified", true
	}
	auditCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.admission.Capture.(llmcapture.TypeResultRecorder).RecordTypeDecision(auditCtx, d.receipt, t.InfoHash.Bytes(), policy); err != nil {
		s.metrics.auditTotal.WithLabelValues("decision_error").Inc()
		return llmcapture.ErrCaptureUnavailable
	}
	s.metrics.auditTotal.WithLabelValues("decision_recorded").Inc()
	return ctx.Err()
}
