package contentfilter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

type auditBudgetProbe struct{ calls int }

func (b *auditBudgetProbe) Reserve(context.Context, int, int) (bool, error) {
	b.calls++
	return true, nil
}

type auditedFakeLLM struct {
	calls atomic.Int32
}

func (f *auditedFakeLLM) Classify(context.Context, string) (LLMVerdict, error) {
	f.calls.Add(1)
	return LLMVerdict{IsEnglish: true, Confidence: 1, Reason: "should-not-run"}, nil
}

func (f *auditedFakeLLM) ClassifyWithResult(context.Context, string) (LLMVerdict, llmcapture.HTTPResult, error) {
	f.calls.Add(1)
	return LLMVerdict{IsEnglish: true, Confidence: 1, Reason: "should-not-run"}, llmcapture.HTTPResult{}, nil
}

type auditCaptureProbe struct {
	mu       sync.Mutex
	request  llmcapture.Request
	result   llmcapture.HTTPResult
	decision llmcapture.ContentFilterDecision
	replay   llmcapture.ContentFilterReplay
	outcome  llmcapture.Outcome
}

func (*auditCaptureProbe) Enabled() bool { return true }
func (p *auditCaptureProbe) Capture(_ context.Context, req llmcapture.Request) (llmcapture.Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.request = req
	if p.outcome != "" {
		return p.outcome, nil
	}
	return llmcapture.OutcomeRecorded, nil
}
func (p *auditCaptureProbe) FindContentFilterReplay(context.Context, []byte, []byte) (llmcapture.ContentFilterReplay, error) {
	return p.replay, nil
}
func (*auditCaptureProbe) RecheckContentFilterRequest(context.Context, []byte, []byte) error {
	return nil
}
func (p *auditCaptureProbe) RecordHTTPResult(_ context.Context, key []byte, result llmcapture.HTTPResult) (llmcapture.ResultReceipt, error) {
	p.mu.Lock()
	p.result = result
	p.mu.Unlock()
	digest := sha256.Sum256(result.Body)
	return llmcapture.ResultReceipt{
		CaptureKey: append([]byte(nil), key...), ResponseSHA256: digest[:],
		FirstObservation: true, StatusCode: result.StatusCode, ErrorClass: result.ErrorClass,
	}, nil
}
func (p *auditCaptureProbe) RecordContentFilterDecision(_ context.Context, _ llmcapture.ResultReceipt, _ []byte, decision llmcapture.ContentFilterDecision) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.decision = decision
	return nil
}

func TestDecideAuditedRetainsExactOpenRouterCallAndShadowDecision(t *testing.T) {
	var providerRequest map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&providerRequest))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"is_english\":false,\"confidence\":0.92,\"reason\":\"spanish-article\"}"}}],"usage":{"prompt_tokens":101,"completion_tokens":17}}`))
	}))
	defer server.Close()

	cfg := phase2Config()
	cfg.LLMApiStyle = apiStyleChat
	cfg.LLMBaseURL = server.URL
	cfg.LLMOpenrouterProvider = "azure/us"
	cfg.LLMMaxOutputTokens = 64
	cfg.LLMMaxRequestBytes = 8192
	cfg.LLMMonthlyBudget = 1500
	cfg.LLMMaxConcurrentCalls = 1
	budget, capture := &auditBudgetProbe{}, &auditCaptureProbe{}
	var promptTokens, completionTokens int
	filter := NewWithLLMAdmission(
		cfg,
		NewOpenAIClientWithPolicy("key", cfg.LLMModel, cfg.LLMBaseURL, cfg.LLMApiStyle,
			cfg.LLMPromptVersion, cfg.LLMOpenrouterProvider, cfg.LLMMaxOutputTokens, time.Second),
		LLMCallbacks{OnLLMTokens: func(_ string, usage TokenUsage) {
			promptTokens, completionTokens = usage.PromptTokens, usage.CompletionTokens
		}},
		Admission{Budget: budget, Capture: capture},
	)
	decision, err := filter.DecideAudited(context.Background(), Input{Title: "Pelicula 2026"}, AuditSource{
		InfoHash: make([]byte, 20), GroupKey: []byte("pelicula 2026"),
	})
	require.NoError(t, err)
	require.True(t, decision.Allow, "shadow mode must not change indexing")
	require.True(t, decision.WouldDrop)
	require.Equal(t, ReasonLLMNonEnglish, decision.Reason)
	require.Equal(t, 1, budget.calls)
	require.Equal(t, 101, promptTokens)
	require.Equal(t, 17, completionTokens)
	require.Equal(t, 200, capture.result.StatusCode)
	require.Equal(t, "none", capture.result.ErrorClass)
	require.Equal(t, "non_english", capture.decision.Outcome)
	require.True(t, capture.decision.WouldDrop)
	require.False(t, capture.decision.Live)
	require.Equal(t, "contentfilter-chat-model-input-v2-openrouter", capture.request.ContractID)
	require.JSONEq(t, string(capture.request.ModelInputJSON), mustJSON(t, providerRequest))
	provider := providerRequest["provider"].(map[string]any)
	require.Equal(t, []any{"azure/us"}, provider["only"])
	require.Equal(t, false, provider["allow_fallbacks"])
	require.Equal(t, "deny", provider["data_collection"])
	require.Equal(t, true, provider["zdr"])
	require.EqualValues(t, 64, providerRequest["max_completion_tokens"])
}

func TestProductionAdmissionCannotFallBackToUnauditedDecide(t *testing.T) {
	cfg := phase2Config()
	llm := &fakeLLM{verdict: LLMVerdict{IsEnglish: false, Confidence: 1, Reason: "foreign"}}
	filter := NewWithLLMAdmission(cfg, llm, LLMCallbacks{}, Admission{
		Budget: &auditBudgetProbe{}, Capture: &auditCaptureProbe{},
	})
	decision := filter.Decide(Input{Title: "Pelicula 2026"})
	require.True(t, decision.Allow)
	require.False(t, decision.WouldDrop)
	require.Zero(t, llm.calls.Load(), "legacy Decide must not bypass durable production admission")
}

func TestAuditedDuplicateReplaysBeforeBudgetOrProvider(t *testing.T) {
	cfg := phase2Config()
	llm := &auditedFakeLLM{}
	budget := &auditBudgetProbe{}
	capture := &auditCaptureProbe{replay: llmcapture.ContentFilterReplay{
		Found: true,
		Decision: &llmcapture.ContentFilterDecision{
			Outcome: "non_english", Confidence: .92, Reason: "spanish-article",
			MinConfidence: cfg.LLMMinConfidenceForDrop, WouldDrop: true,
		},
	}}
	filter := NewWithLLMAdmission(cfg, llm, LLMCallbacks{}, Admission{Budget: budget, Capture: capture})
	decision, err := filter.DecideAudited(context.Background(), Input{Title: "Pelicula 2026"}, AuditSource{
		InfoHash: make([]byte, 20), GroupKey: []byte("pelicula 2026"),
	})
	require.NoError(t, err)
	require.True(t, decision.Allow, "shadow replay must not change indexing")
	require.True(t, decision.WouldDrop)
	require.Equal(t, ReasonLLMNonEnglish, decision.Reason)
	require.Zero(t, budget.calls, "durable duplicate must not consume budget")
	require.Zero(t, llm.calls.Load(), "durable duplicate must not dispatch again")
}

func TestAuditedDuplicateWithoutDecisionCompletesAuditWithoutRetryOrSpend(t *testing.T) {
	cfg := phase2Config()
	llm := &auditedFakeLLM{}
	budget := &auditBudgetProbe{}
	capture := &auditCaptureProbe{replay: llmcapture.ContentFilterReplay{Found: true}}
	filter := NewWithLLMAdmission(cfg, llm, LLMCallbacks{}, Admission{Budget: budget, Capture: capture})
	decision, err := filter.DecideAudited(context.Background(), Input{Title: "Pelicula 2026"}, AuditSource{
		InfoHash: make([]byte, 20), GroupKey: []byte("pelicula 2026"),
	})
	require.NoError(t, err)
	require.True(t, decision.Allow)
	require.Zero(t, budget.calls)
	require.Zero(t, llm.calls.Load())
	require.Equal(t, "audit_incomplete", capture.result.ErrorClass)
	require.Equal(t, "audit_incomplete", capture.decision.Outcome)
}

func TestAuditedDuplicateWithResultCompletesMissingDecisionWithoutProvider(t *testing.T) {
	cfg := phase2Config()
	llm := &auditedFakeLLM{}
	budget := &auditBudgetProbe{}
	capture := &auditCaptureProbe{replay: llmcapture.ContentFilterReplay{
		Found: true,
		Result: &llmcapture.ResultReceipt{
			CaptureKey: make([]byte, 32), ResponseSHA256: make([]byte, 32),
			FirstObservation: true, StatusCode: 200, ErrorClass: "none",
		},
	}}
	filter := NewWithLLMAdmission(cfg, llm, LLMCallbacks{}, Admission{Budget: budget, Capture: capture})
	decision, err := filter.DecideAudited(context.Background(), Input{Title: "Pelicula 2026"}, AuditSource{
		InfoHash: make([]byte, 20), GroupKey: []byte("pelicula 2026"),
	})
	require.NoError(t, err)
	require.True(t, decision.Allow)
	require.Zero(t, budget.calls)
	require.Zero(t, llm.calls.Load())
	require.Equal(t, "audit_incomplete", capture.decision.Outcome)
	require.Empty(t, capture.result.ErrorClass, "existing result must not be replaced")
}

func TestAuditedProviderErrorRetainsTerminalDecision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()
	cfg := phase2Config()
	cfg.LLMApiStyle, cfg.LLMBaseURL = apiStyleChat, server.URL
	cfg.LLMOpenrouterProvider = "azure/us"
	cfg.LLMMonthlyBudget, cfg.LLMMaxConcurrentCalls = 1500, 1
	budget, capture := &auditBudgetProbe{}, &auditCaptureProbe{}
	filter := NewWithLLMAdmission(
		cfg,
		NewOpenAIClientWithPolicy("key", cfg.LLMModel, cfg.LLMBaseURL, cfg.LLMApiStyle,
			cfg.LLMPromptVersion, cfg.LLMOpenrouterProvider, cfg.LLMMaxOutputTokens, time.Second),
		LLMCallbacks{}, Admission{Budget: budget, Capture: capture},
	)
	decision, err := filter.DecideAudited(context.Background(), Input{Title: "Pelicula 2026"}, AuditSource{
		InfoHash: make([]byte, 20), GroupKey: []byte("pelicula 2026"),
	})
	require.NoError(t, err)
	require.True(t, decision.Allow)
	require.Equal(t, 1, budget.calls)
	require.Equal(t, http.StatusTooManyRequests, capture.result.StatusCode)
	require.Equal(t, "http_status", capture.result.ErrorClass)
	require.Equal(t, "invalid_response", capture.decision.Outcome)
	require.False(t, capture.decision.WouldDrop)
}

func TestMissingUsageCountsOnlySuccessfulProviderResponses(t *testing.T) {
	var missing, tokens atomic.Int32
	filter := NewWithLLM(phase2Config(), &fakeLLM{}, LLMCallbacks{
		OnLLMUsageMissing: func(string) { missing.Add(1) },
		OnLLMTokens:       func(string, TokenUsage) { tokens.Add(1) },
	})
	filter.observeLLMCall(LLMVerdict{}, context.DeadlineExceeded, time.Millisecond)
	require.Zero(t, missing.Load(), "failed calls have their own error metric")
	filter.observeLLMCall(LLMVerdict{}, nil, time.Millisecond)
	require.EqualValues(t, 1, missing.Load())
	filter.observeLLMCall(LLMVerdict{Usage: TokenUsage{PromptTokens: 3, CompletionTokens: 1}}, nil, time.Millisecond)
	require.EqualValues(t, 1, missing.Load())
	require.EqualValues(t, 1, tokens.Load())
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return string(raw)
}
