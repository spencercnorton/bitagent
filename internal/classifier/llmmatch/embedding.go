package llmmatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
)

const embeddingShortlistAlgorithm = "embedding-cosine-shortlist-v1"

type embeddingDispatchKey struct{}

type embeddingRequest struct {
	Model          string          `json:"model"`
	Input          []string        `json:"input"`
	EncodingFormat string          `json:"encoding_format"`
	Dimensions     int             `json:"dimensions,omitempty"`
	Provider       *providerPolicy `json:"provider,omitempty"`
}

// This evidence is kept in the chat capture, never in its model-visible input.
// Similarity is a ranking score, not the chat model's confidence probability.
type embeddingShortlistAudit struct {
	Algorithm      string                   `json:"algorithm"`
	Outcome        string                   `json:"outcome"`
	CaptureKey     string                   `json:"capture_key,omitempty"`
	ResponseSHA256 string                   `json:"response_sha256,omitempty"`
	CandidateIDs   []int64                  `json:"candidate_ids,omitempty"`
	Scores         []embeddingRankingScore  `json:"scores,omitempty"`
	Receipt        llmcapture.ResultReceipt `json:"-"`
}

type embeddingRankingScore struct {
	ID            int64 `json:"tmdb_id"`
	SimilarityPPB int64 `json:"similarity_ppb"`
}

func (c EmbeddingConfig) cacheContext() map[string]any {
	return map[string]any{
		"algorithm": embeddingShortlistAlgorithm, "endpoint": c.Endpoint,
		"model": c.Model, "provider": c.OpenrouterProvider,
		"dimensions": c.Dimensions, "max_dimensions": c.MaxDimensions,
		"shortlist_size": c.ShortlistSize,
	}
}

// embeddingShortlist leaves the original list untouched for the classifier's
// independent title/year/type/ambiguity gates. Only the subsequent chat request
// gets the reduced list; its usual confidence floor still owns attachment.
func (c *Client) embeddingShortlist(ctx context.Context, t model.Torrent, ext Extraction,
	candidates []Candidate, source llmcapture.CandidateSource,
) ([]Candidate, *embeddingShortlistAudit, error) {
	cfg := c.cfg.Embeddings
	if !cfg.Enabled || len(candidates) <= cfg.ShortlistSize {
		return candidates, nil, nil
	}
	if err := cfg.validate(); err != nil {
		return nil, nil, err
	}
	if !c.Allow(ctx, t) {
		return nil, nil, llmcapture.ErrPrivacyBlocked
	}
	seenIDs := make(map[int64]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID <= 0 || seenIDs[candidate.ID] {
			return candidates, &embeddingShortlistAudit{Algorithm: embeddingShortlistAlgorithm, Outcome: "invalid_candidates"}, nil
		}
		seenIDs[candidate.ID] = true
	}
	keyInput, err := json.Marshal(map[string]any{
		"info_hash": t.InfoHash.Bytes(), "release_name": t.Name, "extraction": ext,
		"source": source, "candidates": rerankCaptureCandidates(candidates),
		"config": cfg.cacheContext(),
	})
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(keyInput)
	key := "embedding|" + hex.EncodeToString(digest[:])
	if cached, ok := c.cache.Get(key); ok {
		audit := cached.(embeddingShortlistAudit)
		audit.Receipt.FromCache = true
		llmcapture.ResultTraceFrom(ctx).RecordResult(llmcapture.TaskMatcherEmbedding, source, audit.Receipt)
		return candidatesByIDs(candidates, audit.CandidateIDs), &audit, nil
	}
	request := embeddingRequest{
		Model: cfg.Model, EncodingFormat: "float", Dimensions: cfg.Dimensions,
		Input: []string{RerankInput(t.Name, ext, nil)},
	}
	for _, candidate := range candidates {
		// AltTitles and parsedTitle remain independent policy evidence and
		// are deliberately absent from both providers' inputs.
		document, err := json.Marshal(candidate)
		if err != nil {
			return nil, nil, err
		}
		request.Input = append(request.Input, string(document))
	}
	if cfg.OpenrouterProvider != "" {
		request.Provider = &providerPolicy{
			Order: []string{cfg.OpenrouterProvider}, Only: []string{cfg.OpenrouterProvider},
			AllowFallbacks: false, RequireParameters: true, DataCollection: "deny", ZDR: true,
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, nil, err
	}
	audit := &embeddingShortlistAudit{Algorithm: embeddingShortlistAlgorithm, Outcome: "request_size"}
	if len(body) > cfg.MaxRequestBytes {
		c.metrics.gateRejects.WithLabelValues("embedding_request_size").Inc()
		return candidates, audit, nil
	}
	ctx = withPendingCapture(ctx)
	taskInput, err := json.Marshal(map[string]any{
		"release_name": t.Name, "extraction": ext,
		"candidates": rerankCaptureCandidates(candidates),
		"algorithm":  embeddingShortlistAlgorithm, "shortlist_size": cfg.ShortlistSize,
		"max_dimensions": cfg.MaxDimensions,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := c.captureEnvelope(ctx, llmcapture.Request{
		Task: llmcapture.TaskMatcherEmbedding, CandidateSource: source,
		InfoHash: t.InfoHash.Bytes(), NativePrivate: t.Private,
		GroupKey: []byte(fmt.Sprintf("%s\x00%s\x00%d", ext.Type, ext.Title, ext.Year)),
		Model:    cfg.Model, Endpoint: cfg.Endpoint, PromptVersion: embeddingShortlistAlgorithm,
		SystemPrompt: embeddingShortlistAlgorithm, ModelInputJSON: body, TaskInputJSON: taskInput,
		BuildIdentity: llmcapture.CurrentBuildIdentity(), ContractID: embeddingShortlistAlgorithm,
	}); err != nil {
		return nil, nil, err
	}
	dispatchPending, _ := ctx.Value(pendingCaptureContextKey{}).(*pendingCapture)
	var vectors [][]float64
	replayed := false
	if c.dispatch != nil && c.dispatch.Enabled() {
		if dispatchPending == nil {
			return nil, nil, llmcapture.ErrCaptureUnavailable
		}
		binding := llmcapture.DispatchRequest{CaptureKey: dispatchPending.key, SemanticKey: dispatchPending.semanticKey, Task: llmcapture.TaskMatcherEmbedding,
			CandidateSource: source, InfoHash: t.InfoHash.Bytes(), FreshCapture: dispatchPending.outcome == llmcapture.OutcomeRecorded,
			Case: llmwork.CaseFence(ctx)}
		lease, outcome, prepareErr := c.dispatch.Prepare(ctx, binding)
		if prepareErr != nil {
			var deferred *llmcapture.DispatchDeferredError
			if errors.As(prepareErr, &deferred) {
				if binding.Case != nil {
					return nil, nil, llmwork.RecordDeferral(ctx, deferred.Reason, deferred.RetryAfterUTC)
				}
				audit.Outcome = "fallback"
				return candidates, audit, nil
			}
			return nil, nil, fmt.Errorf("%w: embedding dispatch admission: %v", llmcapture.ErrCaptureUnavailable, prepareErr)
		}
		if outcome == llmcapture.DispatchReplay {
			if err := llmwork.BeforeDispatch(ctx); err != nil {
				return nil, nil, err
			}
			response, replayErr := c.dispatch.Replay(ctx, binding)
			if replayErr != nil {
				return nil, nil, replayErr
			}
			llmcapture.ResultTraceFrom(ctx).RecordResult(llmcapture.TaskMatcherEmbedding, source, response.Receipt)
			dispatchPending.key = append([]byte(nil), response.Receipt.CaptureKey...)
			if response.Result.StatusCode == http.StatusOK && response.Result.ErrorClass == "none" {
				vectors, err = decodeEmbeddingVectors(response.Result.Body, len(request.Input), cfg)
			} else {
				err = fmt.Errorf("retained embedding response is not successful")
			}
			replayed = true
		} else if outcome == llmcapture.DispatchPrepared {
			if !bytes.Equal(lease.CaptureKey, dispatchPending.key) {
				return nil, nil, llmwork.ErrHeld
			}
			ctx = context.WithValue(ctx, embeddingDispatchKey{}, lease)
		} else {
			return nil, nil, llmcapture.ErrCaptureUnavailable
		}
	} else if dispatchPending == nil || dispatchPending.outcome != llmcapture.OutcomeRecorded {
		// Concurrent workers/restarts cannot buy a second response for the
		// immutable request. Successful in-process retries use the cache above.
		return nil, nil, fmt.Errorf("%w: embedding request is already captured", llmcapture.ErrCaptureUnavailable)
	}
	if !replayed {
		vectors, err = c.callEmbeddings(ctx, t, body, len(request.Input))
	}
	if errors.Is(err, llmwork.ErrDeferred) {
		return nil, nil, err
	}
	if errors.Is(err, llmcapture.ErrPrivacyBlocked) || errors.Is(err, llmcapture.ErrCaptureUnavailable) {
		return nil, nil, err
	}
	receipt, observed := llmcapture.ResultTraceFrom(ctx).Result(llmcapture.TaskMatcherEmbedding, source)
	pending, _ := ctx.Value(pendingCaptureContextKey{}).(*pendingCapture)
	observed = observed && pending != nil && bytes.Equal(receipt.CaptureKey, pending.key)
	if observed {
		// A duplicate response must not substitute fresh vectors for the
		// immutable first response. A retry in this process replays the cache.
		if !receipt.FirstObservation {
			return nil, nil, fmt.Errorf("%w: embeddings result is not the first observation", llmcapture.ErrCaptureUnavailable)
		}
		audit.CaptureKey = hex.EncodeToString(receipt.CaptureKey)
		audit.ResponseSHA256 = hex.EncodeToString(receipt.ResponseSHA256)
		audit.Receipt = receipt
	}
	if err != nil {
		audit.Outcome = "fallback"
		c.metrics.embeddingShortlists.WithLabelValues("fallback").Inc()
		return candidates, audit, nil
	}
	if !observed {
		return nil, nil, fmt.Errorf("%w: embeddings has no bound result", llmcapture.ErrCaptureUnavailable)
	}
	audit.Outcome = "shortlisted"
	for index, candidate := range candidates {
		var cosine float64
		for component := range vectors[0] {
			cosine += vectors[0][component] * vectors[index+1][component]
		}
		cosine = math.Max(-1, math.Min(1, cosine))
		audit.Scores = append(audit.Scores, embeddingRankingScore{ID: candidate.ID,
			SimilarityPPB: int64(math.Round(cosine * 1_000_000_000))})
	}
	// Stable comparison preserves the frozen candidate order on score ties.
	sort.SliceStable(audit.Scores, func(i, j int) bool {
		return audit.Scores[i].SimilarityPPB > audit.Scores[j].SimilarityPPB
	})
	for _, score := range audit.Scores[:cfg.ShortlistSize] {
		audit.CandidateIDs = append(audit.CandidateIDs, score.ID)
	}
	c.cache.Put(key, *audit)
	c.metrics.embeddingShortlists.WithLabelValues("shortlisted").Inc()
	return candidatesByIDs(candidates, audit.CandidateIDs), audit, nil
}

func (c *Client) embeddingReady() error {
	if err := c.cfg.Embeddings.validate(); err != nil {
		return err
	}
	if c.capture == nil || !c.capture.Enabled() {
		return fmt.Errorf("%w: embeddings requires enabled capture", llmcapture.ErrCaptureUnavailable)
	}
	if _, ok := c.capture.(llmcapture.EmbeddingResultRecorder); !ok {
		return fmt.Errorf("%w: embeddings requires request recheck and result recording", llmcapture.ErrCaptureUnavailable)
	}
	if _, ok := c.capture.(llmcapture.ResultRecorder); !ok {
		return fmt.Errorf("%w: embeddings requires final match recording", llmcapture.ErrCaptureUnavailable)
	}
	return nil
}

func candidatesByIDs(candidates []Candidate, ids []int64) []Candidate {
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		for _, candidate := range candidates {
			if candidate.ID == id {
				out = append(out, candidate)
				break
			}
		}
	}
	return out
}

// callEmbeddings reserves exactly one shared durable slot before dispatch.
// It never retries. Terminal HTTP evidence is retained before fallback to chat.
func (c *Client) callEmbeddings(ctx context.Context, t model.Torrent, body []byte, count int) ([][]float64, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("matcher is disabled")
	}
	if err := c.cfg.Validate(); err != nil {
		return nil, err
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		c.metrics.gateRejects.WithLabelValues("concurrency").Inc()
		if lease, ok := ctx.Value(embeddingDispatchKey{}).(llmcapture.DispatchLease); ok {
			retry := time.Now().UTC().Add(time.Second)
			if err := c.dispatch.DeferNoDispatch(ctx, lease, "concurrency", retry); err != nil {
				return nil, err
			}
			if llmwork.CaseFence(ctx) != nil {
				return nil, llmwork.RecordDeferral(ctx, "concurrency", retry)
			}
		}
		return nil, ErrCallBudget
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Embeddings.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.cfg.Embeddings.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.cfg.Embeddings.APIKey)
	}
	budgetCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	lease, controlled := ctx.Value(embeddingDispatchKey{}).(llmcapture.DispatchLease)
	if c.budget == nil && !controlled {
		cancel()
		c.metrics.budgetSkips.WithLabelValues("unavailable").Inc()
		return nil, ErrCallBudget
	}
	var allowed bool
	if controlled {
		allowed, err = c.dispatch.Reserve(budgetCtx, lease, "matcher", c.cfg.DailyCallLimit, c.cfg.MonthlyCallLimit)
	} else {
		allowed, err = c.budget.Reserve(budgetCtx, c.cfg.DailyCallLimit, c.cfg.MonthlyCallLimit)
	}
	cancel()
	if err != nil || !allowed {
		var deferred *llmcapture.DispatchDeferredError
		if errors.As(err, &deferred) && llmwork.CaseFence(ctx) != nil {
			return nil, llmwork.RecordDeferral(ctx, deferred.Reason, deferred.RetryAfterUTC)
		}
		if controlled && err != nil && deferred == nil {
			return nil, fmt.Errorf("%w: embedding budget admission: %v", llmcapture.ErrCaptureUnavailable, err)
		}
		reason := "exhausted"
		if err != nil {
			reason = "unavailable"
		}
		c.metrics.budgetSkips.WithLabelValues(reason).Inc()
		return nil, ErrCallBudget
	}
	// Recheck the database-bound native/qB privacy admission immediately after
	// reservation. A reservation is never refunded, even when egress is denied.
	pending, _ := ctx.Value(pendingCaptureContextKey{}).(*pendingCapture)
	if pending == nil {
		return nil, llmcapture.ErrCaptureUnavailable
	}
	recheckCtx, recheckCancel := context.WithTimeout(ctx, 2*time.Second)
	err = c.capture.(llmcapture.EmbeddingResultRecorder).RecheckEmbeddingRequest(recheckCtx, pending.key, t.InfoHash.Bytes())
	recheckCancel()
	if err != nil {
		return nil, fmt.Errorf("%w: embedding admission recheck", llmcapture.ErrCaptureUnavailable)
	}
	if controlled {
		if err := llmwork.BeforeDispatch(ctx); err != nil {
			return nil, err
		}
		if err := c.dispatch.BeginDispatch(ctx, lease); err != nil {
			return nil, fmt.Errorf("%w: embedding dispatch fence: %v", llmcapture.ErrCaptureUnavailable, err)
		}
	}
	c.metrics.calls.WithLabelValues(c.cfg.Embeddings.Model, "embedding").Inc()
	start := time.Now()
	response, err := c.httpEmbedding.Do(request)
	result := llmcapture.HTTPResult{ErrorClass: "transport"}
	var vectors [][]float64
	if err == nil {
		defer response.Body.Close()
		result.StatusCode = response.StatusCode
		result.Body, err = io.ReadAll(io.LimitReader(response.Body, llmcapture.MaxResultBodyBytes+1))
		switch {
		case err != nil || len(result.Body) > llmcapture.MaxResultBodyBytes:
			result.Body = result.Body[:min(len(result.Body), llmcapture.MaxResultBodyBytes)]
			result.ErrorClass, err = "read", errors.New("embedding response exceeds bounded readable body")
		case response.StatusCode != http.StatusOK:
			result.ErrorClass, err = "http_status", errors.New("embedding HTTP status is not 200")
		default:
			vectors, err = decodeEmbeddingVectors(result.Body, count, c.cfg.Embeddings)
			result.ErrorClass = "none"
			if err != nil {
				result.ErrorClass = "envelope"
			}
		}
	}
	c.metrics.observeHTTP(c.cfg.Embeddings.Model, "embedding", start, result, err)
	c.recordEmbeddingUsage(result.Body)
	if recordErr := c.recordCapturedResult(ctx, "embedding", result); recordErr != nil {
		return nil, recordErr
	}
	if controlled {
		receipt, ok := llmcapture.ResultTraceFrom(ctx).Result(llmcapture.TaskMatcherEmbedding, pending.source)
		if !ok {
			return nil, llmcapture.ErrCaptureUnavailable
		}
		if err := c.dispatch.ObserveResult(ctx, lease, receipt); err != nil {
			return nil, fmt.Errorf("%w: embedding dispatch receipt: %v", llmcapture.ErrCaptureUnavailable, err)
		}
		if err := llmwork.RecordProgress(ctx); err != nil {
			return nil, err
		}
	}
	if err != nil {
		c.metrics.callErrors.WithLabelValues("embedding", result.ErrorClass).Inc()
	}
	return vectors, err
}

func (c *Client) recordEmbeddingUsage(raw []byte) {
	var response struct {
		Usage *struct {
			PromptTokens *int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Usage == nil ||
		response.Usage.PromptTokens == nil || *response.Usage.PromptTokens < 0 {
		c.metrics.usageMissing.WithLabelValues(c.cfg.Embeddings.Model, "embedding").Inc()
		return
	}
	c.metrics.tokens.WithLabelValues(c.cfg.Embeddings.Model, "embedding", "input").Add(float64(*response.Usage.PromptTokens))
}
