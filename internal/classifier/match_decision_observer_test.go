package classifier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type observerResultCapture struct {
	enabled     bool
	err         error
	calls       int
	receipt     llmcapture.ResultReceipt
	receipts    []llmcapture.ResultReceipt
	infoHash    []byte
	decision    llmcapture.MatchDecision
	ctxErr      error
	deadline    time.Time
	httpCalls   int
	httpReceipt llmcapture.ResultReceipt
}

func (c *observerResultCapture) Enabled() bool { return c.enabled }

func (c *observerResultCapture) Capture(
	context.Context,
	llmcapture.Request,
) (llmcapture.Outcome, error) {
	return llmcapture.OutcomeRecorded, nil
}

func (c *observerResultCapture) RecordHTTPResult(
	_ context.Context,
	captureKey []byte,
	_ llmcapture.HTTPResult,
) (llmcapture.ResultReceipt, error) {
	c.httpCalls++
	receipt := c.httpReceipt
	receipt.CaptureKey = append([]byte(nil), captureKey...)
	return receipt, nil
}

func (c *observerResultCapture) RecordMatchDecision(
	ctx context.Context,
	receipt llmcapture.ResultReceipt,
	infoHash []byte,
	decision llmcapture.MatchDecision,
) error {
	c.calls++
	c.receipt = receipt
	c.receipts = append(c.receipts, receipt)
	c.infoHash = infoHash
	c.decision = decision
	c.ctxErr = ctx.Err()
	c.deadline, _ = ctx.Deadline()
	return c.err
}

type observerCaptureOnly struct{ enabled bool }

func (c observerCaptureOnly) Enabled() bool { return c.enabled }

func (c observerCaptureOnly) Capture(
	context.Context,
	llmcapture.Request,
) (llmcapture.Outcome, error) {
	return llmcapture.OutcomeRecorded, nil
}

func newObserverTestMatcher() (*llmmatch.Client, *llmmatch.Metrics) {
	metrics := llmmatch.NewMetrics()
	return llmmatch.NewClient(
		llmmatch.NewDefaultConfig(), nil, metrics, zap.NewNop().Sugar(),
	), metrics
}

func observerContext(receipt llmcapture.ResultReceipt, source llmcapture.CandidateSource) context.Context {
	ctx, trace := llmcapture.WithResultTrace(context.Background())
	trace.RecordResult(llmcapture.TaskMatcherRerank, source, receipt)
	return ctx
}

func firstObserverReceipt() llmcapture.ResultReceipt {
	return llmcapture.ResultReceipt{
		CaptureKey:       make([]byte, 32),
		ResponseSHA256:   append([]byte{1}, make([]byte, 31)...),
		FirstObservation: true,
		StatusCode:       200,
		ErrorClass:       "none",
	}
}

func TestCaptureMatchDecisionObserverMapsCompactDecisionAndUsesCleanupContext(t *testing.T) {
	capture := &observerResultCapture{enabled: true}
	matcher, metrics := newObserverTestMatcher()
	observer := NewMatchDecisionObserver(capture, matcher)
	receipt := firstObserverReceipt()
	baseCtx := observerContext(receipt, llmcapture.CandidateSourceAPI)
	ctx, cancel := context.WithCancel(baseCtx)
	cancel()
	infoHash := append([]byte{7}, make([]byte, 19)...)
	observation := MatchDecisionObservation{
		InfoHash:           infoHash,
		Outcome:            OutcomeDeclined,
		Extract:            llmmatch.Extraction{Title: "not persisted"},
		ParsedTitle:        "not persisted",
		CandidateSource:    "api",
		Candidates:         []llmmatch.Candidate{{ID: 42, Title: "not persisted"}},
		MatchedID:          42,
		MatchedTitle:       "not persisted",
		Confidence:         0.93,
		MinConfidence:      0.8,
		RequireSourceTitle: true,
		GateReason:         "source_title",
		Resolved:           true,
		ResolvedYear:       2024,
		WouldAttach:        false,
		Live:               false,
	}

	require.NoError(t, observer.ObserveLLMMatchDecision(ctx, observation))
	require.Equal(t, 1, capture.calls)
	assert.Equal(t, receipt, capture.receipt)
	assert.Equal(t, infoHash, capture.infoHash)
	assert.Equal(t, llmcapture.MatchDecision{
		Outcome:            "declined",
		GateReason:         "source_title",
		ChosenID:           42,
		Confidence:         0.93,
		ResolvedYear:       2024,
		MinConfidence:      0.8,
		RequireSourceTitle: true,
	}, capture.decision)
	assert.NoError(t, capture.ctxErr, "canceled action context must not cancel the bounded ledger write")
	remaining := time.Until(capture.deadline)
	assert.Greater(t, remaining, time.Duration(0))
	assert.LessOrEqual(t, remaining, matchDecisionWriteTimeout)
	infoHash[0] = 99
	assert.Equal(t, byte(7), capture.infoHash[0], "recorder input must not alias observation storage")
	assert.Equal(t, float64(1), auditDecisionMetric(t, metrics, "recorded"))
}

func TestCaptureMatchDecisionObserverNoProviderResultDoesNotFabricateBinding(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		finalSource string
	}{
		{name: "cache or pre-rerank", ctx: context.Background(), finalSource: "api"},
		{
			name:        "local result cannot bind later api decision",
			ctx:         observerContext(firstObserverReceipt(), llmcapture.CandidateSourceLocal),
			finalSource: "api",
		},
		{
			name:        "alias has no rerank provider result",
			ctx:         observerContext(firstObserverReceipt(), llmcapture.CandidateSourceAPI),
			finalSource: "alias",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &observerResultCapture{enabled: true}
			matcher, metrics := newObserverTestMatcher()
			observer := NewMatchDecisionObserver(capture, matcher)
			require.NoError(t, observer.ObserveLLMMatchDecision(tc.ctx, MatchDecisionObservation{
				InfoHash: make([]byte, 20), CandidateSource: tc.finalSource,
			}))
			assert.Zero(t, capture.calls)
			assert.Equal(t, float64(1), auditDecisionMetric(t, metrics, "no_provider_result"))
		})
	}
}

func TestCaptureMatchDecisionObserverCannotAttachWithoutSuccessfulBoundReceipt(t *testing.T) {
	bad := firstObserverReceipt()
	bad.ErrorClass = "read"
	for _, ctx := range []context.Context{context.Background(), observerContext(bad, llmcapture.CandidateSourceAPI)} {
		capture := &observerResultCapture{enabled: true}
		matcher, _ := newObserverTestMatcher()
		observer := NewMatchDecisionObserver(capture, matcher)
		err := observer.ObserveLLMMatchDecision(ctx, MatchDecisionObservation{CandidateSource: "api", WouldAttach: true, Live: true})
		require.ErrorIs(t, err, llmcapture.ErrCaptureUnavailable)
		require.Zero(t, capture.calls)
	}
}

func TestCaptureMatchDecisionObserverOffersDuplicateForIdempotentValidation(t *testing.T) {
	receipt := firstObserverReceipt()
	receipt.FirstObservation = false
	capture := &observerResultCapture{enabled: true}
	matcher, metrics := newObserverTestMatcher()
	observer := NewMatchDecisionObserver(capture, matcher)
	require.NoError(t, observer.ObserveLLMMatchDecision(
		observerContext(receipt, llmcapture.CandidateSourceAPI),
		MatchDecisionObservation{
			InfoHash: make([]byte, 20), Outcome: OutcomeMatched,
			CandidateSource: "api", MatchedID: 42, Confidence: 0.9,
			MinConfidence: 0.8, WouldAttach: true,
		},
	))
	assert.Equal(t, 1, capture.calls, "recorder must validate duplicate/idempotent decision binding")
	assert.Equal(t, float64(1), auditDecisionMetric(t, metrics, "duplicate"))
	assert.Zero(t, auditDecisionMetric(t, metrics, "recorded"))
}

func TestCaptureMatchDecisionObserverSkipsResponseErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		receipt llmcapture.ResultReceipt
	}{
		{
			name: "http error",
			receipt: func() llmcapture.ResultReceipt {
				r := firstObserverReceipt()
				r.StatusCode = 503
				r.ErrorClass = "http_status"
				return r
			}(),
		},
		{
			name: "structured response error",
			receipt: func() llmcapture.ResultReceipt {
				r := firstObserverReceipt()
				r.ErrorClass = "envelope"
				return r
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &observerResultCapture{enabled: true}
			matcher, metrics := newObserverTestMatcher()
			observer := NewMatchDecisionObserver(capture, matcher)
			require.NoError(t, observer.ObserveLLMMatchDecision(
				observerContext(tc.receipt, llmcapture.CandidateSourceAPI),
				MatchDecisionObservation{CandidateSource: "api"},
			))
			assert.Zero(t, capture.calls)
			assert.Equal(t, float64(1), auditDecisionMetric(t, metrics, "response_error"))
		})
	}
}

func TestCaptureMatchDecisionObserverPersistenceFailureFailsClosed(t *testing.T) {
	wantErr := errors.New("database unavailable")
	capture := &observerResultCapture{enabled: true, err: wantErr}
	matcher, metrics := newObserverTestMatcher()
	observer := NewMatchDecisionObserver(capture, matcher)
	err := observer.ObserveLLMMatchDecision(
		observerContext(firstObserverReceipt(), llmcapture.CandidateSourceLocal),
		MatchDecisionObservation{
			InfoHash: make([]byte, 20), Outcome: OutcomeMatched,
			CandidateSource: "local", MatchedID: 1, Confidence: 0.9,
			MinConfidence: 0.8, WouldAttach: true,
		},
	)
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, capture.calls)
	assert.Equal(t, float64(1), auditDecisionMetric(t, metrics, "error"))
}

func TestCaptureMatchDecisionObserverRetryUsesCachedFirstReceipt(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"content": `{"tmdb_id":42,"confidence":0.93}`,
				},
			}},
		}); err != nil {
			t.Errorf("encode matcher response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	wantErr := errors.New("first decision write failed")
	receipt := firstObserverReceipt()
	capture := &observerResultCapture{
		enabled: true, err: wantErr, httpReceipt: receipt,
	}
	metrics := llmmatch.NewMetrics()
	cfg := llmmatch.NewDefaultConfig()
	cfg.Enabled = true
	cfg.Endpoint = server.URL
	client := llmmatch.NewClientWithCapture(
		cfg, nil, metrics, zap.NewNop().Sugar(), capture,
	)
	observer := NewMatchDecisionObserver(capture, client)
	torrent := model.Torrent{Name: "Dune.2021.1080p.mkv"}
	extraction := llmmatch.Extraction{Title: "Dune", Year: 2021, Type: "movie", OK: true}
	candidates := []llmmatch.Candidate{{ID: 42, Title: "Dune", Year: 2021}}
	observation := MatchDecisionObservation{
		InfoHash: make([]byte, 20), Outcome: OutcomeMatched,
		CandidateSource: "api", MatchedID: 42, Confidence: 0.93,
		MinConfidence: cfg.MinConfidence, WouldAttach: true,
	}

	firstCtx, _ := llmcapture.WithResultTrace(context.Background())
	id, confidence, err := client.RerankForMediaType(
		firstCtx, torrent, extraction, "Dune", false, candidates,
		llmcapture.CandidateSourceAPI,
	)
	require.NoError(t, err)
	require.EqualValues(t, 42, id)
	require.Equal(t, 0.93, confidence)
	assert.ErrorIs(t, observer.ObserveLLMMatchDecision(firstCtx, observation), wantErr)

	capture.err = nil
	retryCtx, retryTrace := llmcapture.WithResultTrace(context.Background())
	id, confidence, err = client.RerankForMediaType(
		retryCtx, torrent, extraction, "Dune", false, candidates,
		llmcapture.CandidateSourceAPI,
	)
	require.NoError(t, err)
	require.EqualValues(t, 42, id)
	require.Equal(t, 0.93, confidence)
	replayed, ok := retryTrace.Result(
		llmcapture.TaskMatcherRerank, llmcapture.CandidateSourceAPI,
	)
	require.True(t, ok)
	require.True(t, replayed.FirstObservation)
	require.True(t, replayed.FromCache)
	require.NoError(t, observer.ObserveLLMMatchDecision(retryCtx, observation))

	assert.EqualValues(t, 1, providerCalls.Load(), "decision retry must not spend another provider call")
	assert.Equal(t, 1, capture.httpCalls)
	require.Len(t, capture.receipts, 2)
	assert.False(t, capture.receipts[0].FromCache)
	assert.True(t, capture.receipts[1].FromCache)
	assert.Equal(t, float64(1), auditDecisionMetric(t, metrics, "error"))
	assert.Equal(t, float64(1), auditDecisionMetric(t, metrics, "recorded"))
}

func TestNewMatchDecisionObserverOptionalAndFailClosedCaptureModes(t *testing.T) {
	matcher, _ := newObserverTestMatcher()
	ctx := observerContext(firstObserverReceipt(), llmcapture.CandidateSourceAPI)
	observation := MatchDecisionObservation{CandidateSource: "api"}

	for _, capture := range []llmcapture.Capturer{
		nil,
		&observerResultCapture{enabled: false},
	} {
		observer := NewMatchDecisionObserver(capture, matcher)
		require.NoError(t, observer.ObserveLLMMatchDecision(ctx, observation))
	}

	observer := NewMatchDecisionObserver(observerCaptureOnly{enabled: true}, matcher)
	err := observer.ObserveLLMMatchDecision(context.Background(), observation)
	assert.ErrorIs(t, err, llmcapture.ErrCaptureUnavailable)
}

func auditDecisionMetric(t *testing.T, metrics *llmmatch.Metrics, outcome string) float64 {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	for _, collector := range metrics.Collectors() {
		require.NoError(t, registry.Register(collector))
	}
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "bitagent_classifier_llm_match_audit_decisions_total" {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "outcome" && label.GetValue() == outcome {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
