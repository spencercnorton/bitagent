package contentfilter

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
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

type reviewBudgetProbe struct{ allowed bool }

func (b reviewBudgetProbe) Reserve(context.Context, int, int) (bool, error) { return b.allowed, nil }

func reviewFilter(t *testing.T, cfg Config, raw string, status int, capture *auditCaptureProbe, budget CallBudget) (*Filter, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(raw))
	}))
	t.Cleanup(server.Close)
	cfg.LLMApiStyle, cfg.LLMBaseURL = apiStyleChat, server.URL
	client := NewOpenAIClientWithPolicy("test", cfg.LLMModel, server.URL, apiStyleChat, cfg.LLMPromptVersion, "", cfg.LLMMaxOutputTokens, time.Second)
	return NewWithLLMAdmission(cfg, client, LLMCallbacks{}, Admission{Capture: capture, Budget: budget}), calls
}

const reviewNegativeResponse = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"is_english\":false,\"confidence\":0.93,\"reason\":\"spanish-article\"}"}}]}`

func TestAuditedReviewKeepsAndBindsDispositionWithoutDrop(t *testing.T) {
	for _, tc := range []struct {
		name, action, live               string
		allow, drop, review, wouldReview bool
	}{
		{"live review", LLMActionReview, "true", true, false, true, true},
		{"shadow review", LLMActionReview, "false", true, false, false, true},
		{"legacy live drop", "", "true", false, true, false, false},
		{"shadow drop", LLMActionDrop, "false", true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := phase2Config()
			cfg.LLMAction, cfg.LLMEnforce = tc.action, tc.live
			capture := &auditCaptureProbe{}
			filter, calls := reviewFilter(t, cfg, reviewNegativeResponse, 200, capture, reviewBudgetProbe{true})
			d, err := filter.DecideAudited(context.Background(), Input{Title: "Pelicula 2026"}, AuditSource{InfoHash: make([]byte, 20), GroupKey: []byte("synthetic")})
			require.NoError(t, err)
			require.Equal(t, tc.allow, d.Allow)
			require.Equal(t, tc.drop, d.WouldDrop)
			require.Equal(t, tc.review, d.Review)
			require.Equal(t, tc.wouldReview, d.WouldReview)
			require.False(t, d.Defer)
			require.EqualValues(t, 1, calls.Load())
			require.Equal(t, cfg.EffectiveLLMAction(), capture.decision.LLMAction)
			require.Equal(t, tc.drop, capture.decision.WouldDrop)
			require.Equal(t, tc.wouldReview, capture.decision.WouldReview)
			var task struct {
				LLMAction string `json:"llm_action"`
			}
			require.NoError(t, json.Unmarshal(capture.request.TaskInputJSON, &task))
			require.Equal(t, cfg.EffectiveLLMAction(), task.LLMAction)
		})
	}
}

func TestReviewRequiresAuditAndKeepsProtectedOrUnusableInputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     Input
		raw       string
		status    int
		budget    bool
		capture   *auditCaptureProbe
		wantCalls int32
		wantErr   bool
	}{
		{"advertised English", Input{Title: "Pelicula ENG 2026"}, reviewNegativeResponse, 200, true, &auditCaptureProbe{}, 0, false},
		{"native private", Input{Title: "Pelicula 2026", Private: true}, reviewNegativeResponse, 200, true, &auditCaptureProbe{}, 0, false},
		{"English answer", Input{Title: "Pelicula 2026"}, `{"choices":[{"message":{"content":"{\"is_english\":true,\"confidence\":0.93,\"reason\":\"english-clear\"}"}}]}`, 200, true, &auditCaptureProbe{}, 1, false},
		{"low confidence", Input{Title: "Pelicula 2026"}, `{"choices":[{"message":{"content":"{\"is_english\":false,\"confidence\":0.2,\"reason\":\"ambiguous\"}"}}]}`, 200, true, &auditCaptureProbe{}, 1, false},
		{"malformed", Input{Title: "Pelicula 2026"}, `{"choices":[{"message":{"content":"{\"is_english\":false}"}}]}`, 200, true, &auditCaptureProbe{}, 1, false},
		{"provider unavailable never defers review", Input{Title: "Pelicula 2026"}, `{"error":"unavailable"}`, 503, true, &auditCaptureProbe{}, 1, false},
		{"budget exhausted", Input{Title: "Pelicula 2026"}, reviewNegativeResponse, 200, false, &auditCaptureProbe{}, 0, false},
		{"privacy revoked", Input{Title: "Pelicula 2026"}, reviewNegativeResponse, 200, true, &auditCaptureProbe{recheckErr: errors.New("private")}, 0, true},
		{"result audit failed", Input{Title: "Pelicula 2026"}, reviewNegativeResponse, 200, true, &auditCaptureProbe{resultErr: errors.New("capture unavailable")}, 1, true},
		{"decision audit failed", Input{Title: "Pelicula 2026"}, reviewNegativeResponse, 200, true, &auditCaptureProbe{decisionErr: errors.New("capture unavailable")}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := phase2Config()
			cfg.LLMAction, cfg.LLMEnforce, cfg.LLMDeferOnUnavailable = LLMActionReview, "true", true
			filter, calls := reviewFilter(t, cfg, tc.raw, tc.status, tc.capture, reviewBudgetProbe{tc.budget})
			d, err := filter.DecideAudited(context.Background(), tc.input, AuditSource{InfoHash: make([]byte, 20), GroupKey: []byte("synthetic")})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.True(t, d.Allow)
			}
			require.False(t, d.Review)
			require.False(t, d.WouldReview)
			require.False(t, d.WouldDrop)
			require.False(t, d.Defer)
			require.Equal(t, tc.wantCalls, calls.Load())
		})
	}
	// A direct client cannot turn its title-only cache into a persisted tag.
	cfg := phase2Config()
	cfg.LLMAction, cfg.LLMEnforce = LLMActionReview, "true"
	llm := &fakeLLM{verdict: LLMVerdict{Confidence: 1, Reason: "foreign"}}
	d := NewWithLLM(cfg, llm, LLMCallbacks{}).Decide(Input{Title: "Pelicula 2026"})
	require.True(t, d.Allow)
	require.False(t, d.Review)
	require.Zero(t, llm.calls.Load())
}

func TestReviewReplayCannotUseDropOrMismatchedPolicy(t *testing.T) {
	cfg := phase2Config()
	cfg.LLMAction, cfg.LLMEnforce = LLMActionReview, "true"
	base := llmcapture.ContentFilterDecision{Outcome: "non_english", Confidence: .93, Reason: "spanish-article", MinConfidence: cfg.LLMMinConfidenceForDrop, Live: true, LLMAction: "review", WouldReview: true}
	for _, mutate := range []func(*llmcapture.ContentFilterDecision){
		func(d *llmcapture.ContentFilterDecision) { d.LLMAction, d.WouldDrop, d.WouldReview = "", true, false },
		func(d *llmcapture.ContentFilterDecision) { d.Live = false },
		func(d *llmcapture.ContentFilterDecision) { d.MinConfidence = .9 },
		func(d *llmcapture.ContentFilterDecision) { d.WouldDrop = true },
	} {
		d := base
		mutate(&d)
		capture := &auditCaptureProbe{replay: llmcapture.ContentFilterReplay{Found: true, Decision: &d}}
		filter, calls := reviewFilter(t, cfg, reviewNegativeResponse, 200, capture, reviewBudgetProbe{true})
		result, err := filter.DecideAudited(context.Background(), Input{Title: "Pelicula 2026"}, AuditSource{InfoHash: make([]byte, 20), GroupKey: []byte("synthetic")})
		require.NoError(t, err)
		require.True(t, result.Allow)
		require.False(t, result.Review)
		require.Zero(t, calls.Load())
	}
}

func TestAuditedReviewReplayPreservesReviewWithoutProviderOrBudget(t *testing.T) {
	cfg := phase2Config()
	cfg.LLMAction, cfg.LLMEnforce = LLMActionReview, "true"
	capture := &auditCaptureProbe{replay: llmcapture.ContentFilterReplay{
		Found:    true,
		Result:   &llmcapture.ResultReceipt{CaptureKey: make([]byte, 32), ResponseSHA256: make([]byte, 32), StatusCode: 200, ErrorClass: "none"},
		Decision: &llmcapture.ContentFilterDecision{Outcome: "non_english", Confidence: .93, Reason: "spanish-article", MinConfidence: cfg.LLMMinConfidenceForDrop, Live: true, LLMAction: "review", WouldReview: true},
	}}
	budget := &auditBudgetProbe{}
	filter, calls := reviewFilter(t, cfg, reviewNegativeResponse, 200, capture, budget)
	d, err := filter.DecideAudited(context.Background(), Input{Title: "Pelicula 2026"}, AuditSource{InfoHash: make([]byte, 20), GroupKey: []byte("synthetic")})
	require.NoError(t, err)
	require.True(t, d.Allow)
	require.True(t, d.Review)
	require.True(t, d.WouldReview)
	require.False(t, d.WouldDrop)
	require.Zero(t, calls.Load())
	require.Zero(t, budget.calls)
}

func TestReviewDoesNotChangeDeterministicPolicyOrDropMetrics(t *testing.T) {
	cfg := phase2Config()
	cfg.LLMAction, cfg.LLMEnforce, cfg.Enforce = LLMActionReview, "true", true
	d := NewWithLLM(cfg, &fakeLLM{}, LLMCallbacks{}).Decide(Input{Title: "Software", PrimaryExtension: "iso"})
	require.False(t, d.Allow)
	require.True(t, d.WouldDrop)
	require.False(t, d.WouldReview)
	require.Equal(t, ReasonBlockedExtension, d.Reason)
	m := NewMetrics()
	registry := prometheus.NewRegistry()
	registry.MustRegister(m.Collectors()...)
	m.Observe(Decision{Allow: true, WouldReview: true, Review: true, Reason: ReasonLLMNonEnglish})
	m.Observe(Decision{Allow: true, WouldReview: true, Reason: ReasonLLMNonEnglish})
	families, err := registry.Gather()
	require.NoError(t, err)
	values := map[string]float64{}
	for _, family := range families {
		if len(family.Metric) == 1 {
			values[family.GetName()] = family.Metric[0].GetCounter().GetValue()
		}
	}
	require.Equal(t, 1.0, values["bitagent_contentfilter_llm_review_total"])
	require.Equal(t, 1.0, values["bitagent_contentfilter_llm_would_review_total"])
	require.Equal(t, 2.0, values["bitagent_contentfilter_keep_total"])
	require.NotContains(t, values, "bitagent_contentfilter_drop_total")
	require.NotContains(t, values, "bitagent_contentfilter_would_drop_total")
}
