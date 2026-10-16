package contentfilter

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

func enforcementSource(value byte) AuditSource {
	hash := make([]byte, 20)
	hash[0] = value
	return AuditSource{InfoHash: hash, GroupKey: []byte("synthetic feature")}
}

func enforcementHTTPFilter(cfg Config, serverURL string, capture *auditCaptureProbe, budget CallBudget) *Filter {
	cfg.LLMApiStyle = apiStyleChat
	cfg.LLMBaseURL = serverURL
	return NewWithLLMAdmission(cfg,
		NewOpenAIClientWithPolicy("test", cfg.LLMModel, serverURL, apiStyleChat,
			cfg.LLMPromptVersion, "", cfg.LLMMaxOutputTokens, time.Second),
		LLMCallbacks{}, Admission{Capture: capture, Budget: budget})
}

func writeEnforcementVerdict(w http.ResponseWriter, verdict string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": verdict}}},
	})
}

func TestAuditedEnforcementModesAreIndependent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEnforcementVerdict(w, `{"is_english":false,"confidence":0.98,"reason":"synthetic-foreign"}`)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name          string
		deterministic bool
		mode          string
		modelLive     bool
	}{
		{"shadow", false, "false", false},
		{"deterministic only", true, "false", false},
		{"model only", false, "true", true},
		{"both", true, "true", true},
		{"legacy live inherited", true, "inherit", true},
		{"legacy live unset", true, "", true},
		{"legacy default remains shadow", false, "inherit", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := phase2Config()
			cfg.Enforce, cfg.LLMEnforce = tc.deterministic, tc.mode
			capture, budget := &auditCaptureProbe{}, &auditBudgetProbe{}
			filter := enforcementHTTPFilter(cfg, server.URL, capture, budget)
			deterministic := filter.DecideDeterministic(Input{Title: "Synthetic installer", PrimaryExtension: "exe"})
			require.Equal(t, !tc.deterministic, deterministic.Allow)
			require.True(t, deterministic.WouldDrop)
			require.Equal(t, ReasonBlockedExtension, deterministic.Reason)
			require.Zero(t, budget.calls, "pre-classifier policy must never spend model budget")
			model, err := filter.DecideAudited(context.Background(), Input{Title: "Synthetic Feature 2026"}, enforcementSource(1))
			require.NoError(t, err)
			require.Equal(t, !tc.modelLive, model.Allow)
			require.True(t, model.WouldDrop)
			require.Equal(t, ReasonLLMNonEnglish, model.Reason)
			require.Equal(t, tc.modelLive, capture.decision.Live)
			var taskInput map[string]any
			require.NoError(t, json.Unmarshal(capture.request.TaskInputJSON, &taskInput))
			require.Equal(t, tc.modelLive, taskInput["live"], "capture must bind the actual model application mode")
		})
	}
}

func TestAuditedModelModeStillRequiresBothEnableFlags(t *testing.T) {
	for _, disabled := range []string{"filter", "model"} {
		t.Run(disabled, func(t *testing.T) {
			cfg := phase2Config()
			cfg.LLMEnforce = "true"
			if disabled == "filter" {
				cfg.Enabled = false
			} else {
				cfg.LLMEnabled = false
			}
			client := &auditedFakeLLM{}
			budget := &auditBudgetProbe{}
			filter := NewWithLLMAdmission(cfg, client, LLMCallbacks{}, Admission{Budget: budget, Capture: &auditCaptureProbe{}})
			decision, err := filter.DecideAudited(context.Background(), Input{Title: "Synthetic Feature 2026"}, enforcementSource(1))
			require.NoError(t, err)
			require.True(t, decision.Allow)
			require.False(t, decision.WouldDrop)
			require.Zero(t, client.calls.Load())
			require.Zero(t, budget.calls)
		})
	}
}

func TestAuditedUnavailableDeferralUsesModelMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "synthetic unavailable", http.StatusTooManyRequests)
	}))
	defer server.Close()
	for _, tc := range []struct {
		mode                     string
		deterministic, deferWant bool
	}{
		{"false", true, false}, {"true", false, true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cfg := phase2Config()
			cfg.Enforce, cfg.LLMEnforce, cfg.LLMDeferOnUnavailable = tc.deterministic, tc.mode, true
			capture := &auditCaptureProbe{}
			decision, err := enforcementHTTPFilter(cfg, server.URL, capture, &auditBudgetProbe{}).DecideAudited(
				context.Background(), Input{Title: "Synthetic Feature 2026"}, enforcementSource(1))
			require.NoError(t, err)
			require.Equal(t, tc.deferWant, decision.Defer)
			require.Equal(t, !tc.deferWant, decision.Allow)
			require.False(t, decision.WouldDrop, "an unavailable provider never supplies a drop")
			require.Equal(t, "invalid_response", capture.decision.Outcome)
		})
	}
}

func TestAuditedMalformedResponsesRemainKeepsInModelLiveMode(t *testing.T) {
	for _, text := range []string{
		`{"confidence":0.99,"reason":"synthetic-foreign"}`,
		`{"is_english":false,"confidence":0.99,"reason":null}`,
		`{"is_english":true,"is_english":false,"confidence":0.99,"reason":"synthetic-foreign"}`,
		`{"is_english":false,"confidence":0.99,"reason":"line\nbreak"}`,
		`{"is_english":false,"confidence":0.99,"reason":"synthetic-foreign"} {}`,
	} {
		t.Run(text, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeEnforcementVerdict(w, text) }))
			defer server.Close()
			cfg := phase2Config()
			cfg.LLMEnforce, cfg.LLMDeferOnUnavailable = "true", true
			capture := &auditCaptureProbe{}
			decision, err := enforcementHTTPFilter(cfg, server.URL, capture, &auditBudgetProbe{}).DecideAudited(
				context.Background(), Input{Title: "Synthetic Feature 2026"}, enforcementSource(1))
			require.NoError(t, err)
			require.True(t, decision.Allow)
			require.False(t, decision.WouldDrop)
			require.False(t, decision.Defer, "malformed data cannot enter an unavailable-provider retry loop")
			require.Equal(t, "invalid_response", capture.decision.Outcome)
			require.False(t, capture.decision.WouldDrop)
		})
	}
}

type enforcementBudget struct {
	denied bool
	err    error
	calls  int
}

func (b *enforcementBudget) Reserve(context.Context, int, int) (bool, error) {
	b.calls++
	return !b.denied, b.err
}

func TestAuditedModelPrivacyAndBudgetGatesCannotDrop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		private bool
		budget  *enforcementBudget
		capture *auditCaptureProbe
		wantErr bool
	}{
		{"native privacy", true, &enforcementBudget{}, &auditCaptureProbe{}, false},
		{"budget exhausted", false, &enforcementBudget{denied: true}, &auditCaptureProbe{}, false},
		{"budget unavailable", false, &enforcementBudget{err: errors.New("synthetic budget unavailable")}, &auditCaptureProbe{}, true},
		{"privacy changed before dispatch", false, &enforcementBudget{}, &auditCaptureProbe{recheckErr: errors.New("synthetic private source")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := phase2Config()
			cfg.LLMEnforce = "true"
			client := &auditedFakeLLM{}
			filter := NewWithLLMAdmission(cfg, client, LLMCallbacks{}, Admission{Budget: tc.budget, Capture: tc.capture})
			decision, err := filter.DecideAudited(context.Background(), Input{Title: "Synthetic Feature 2026", Private: tc.private}, enforcementSource(1))
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.True(t, decision.Allow)
			}
			require.False(t, decision.WouldDrop)
			require.False(t, decision.Defer)
			require.Zero(t, client.calls.Load(), "a failed admission gate must prevent dispatch")
		})
	}
}

func TestAuditedNewSourceCannotUseTitleCacheToBypassPrivacy(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		writeEnforcementVerdict(w, `{"is_english":false,"confidence":0.98,"reason":"synthetic-foreign"}`)
	}))
	defer server.Close()
	cfg := phase2Config()
	cfg.LLMEnforce = "true"
	capture := &auditCaptureProbe{}
	filter := enforcementHTTPFilter(cfg, server.URL, capture, &auditBudgetProbe{})
	first, err := filter.DecideAudited(context.Background(), Input{Title: "Synthetic Feature 2026"}, enforcementSource(1))
	require.NoError(t, err)
	require.False(t, first.Allow)
	// Even a pre-existing direct cache entry cannot replace the next source's
	// bound capture and privacy admission check.
	filter.cache.Put(llmCacheKey(cfg.LLMModel, cfg.LLMPromptVersion, normalizeTitle("Synthetic Feature 2026")),
		LLMVerdict{IsEnglish: false, Confidence: 1, Reason: "synthetic-foreign"})
	capture.recheckErr = errors.New("synthetic private source")
	second, err := filter.DecideAudited(context.Background(), Input{Title: "Synthetic Feature 2026"}, enforcementSource(2))
	require.ErrorIs(t, err, llmcapture.ErrCaptureUnavailable)
	require.False(t, second.WouldDrop)
	require.Equal(t, 2, capture.captures, "each source requires its own durable capture")
	require.EqualValues(t, 1, providerCalls.Load(), "private source must not dispatch")
}

type enforcementResultClient struct {
	verdict LLMVerdict
	result  llmcapture.HTTPResult
}

func (c *enforcementResultClient) Classify(context.Context, string) (LLMVerdict, error) {
	return c.verdict, nil
}
func (c *enforcementResultClient) ClassifyWithResult(context.Context, string) (LLMVerdict, llmcapture.HTTPResult, error) {
	return c.verdict, c.result, nil
}

func TestAuditedInvalidResultMetadataCannotApplyNominalVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict LLMVerdict
		result  llmcapture.HTTPResult
	}{
		{"http failure", LLMVerdict{Confidence: .99, Reason: "synthetic-foreign"}, llmcapture.HTTPResult{StatusCode: 500, ErrorClass: "none"}},
		{"error class", LLMVerdict{Confidence: .99, Reason: "synthetic-foreign"}, llmcapture.HTTPResult{StatusCode: 200, ErrorClass: "transport"}},
		{"NaN verdict", LLMVerdict{Confidence: math.NaN(), Reason: "synthetic-foreign"}, llmcapture.HTTPResult{StatusCode: 200, ErrorClass: "none"}},
		{"invalid reason", LLMVerdict{Confidence: .99, Reason: strings.Repeat("x", 65)}, llmcapture.HTTPResult{StatusCode: 200, ErrorClass: "none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := phase2Config()
			cfg.LLMEnforce = "true"
			capture := &auditCaptureProbe{}
			filter := NewWithLLMAdmission(cfg, &enforcementResultClient{verdict: tc.verdict, result: tc.result}, LLMCallbacks{}, Admission{Budget: &auditBudgetProbe{}, Capture: capture})
			decision, err := filter.DecideAudited(context.Background(), Input{Title: "Synthetic Feature 2026"}, enforcementSource(1))
			require.NoError(t, err)
			require.True(t, decision.Allow)
			require.False(t, decision.WouldDrop)
			require.Equal(t, "invalid_response", capture.decision.Outcome)
			require.Zero(t, capture.decision.Confidence)
		})
	}
}

func TestAuditedIncompleteOrInvalidReplayCannotDrop(t *testing.T) {
	for _, outcome := range []string{"invalid_response", "audit_incomplete", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			cfg := phase2Config()
			cfg.LLMEnforce = "true"
			capture := &auditCaptureProbe{replay: llmcapture.ContentFilterReplay{Found: true, Decision: &llmcapture.ContentFilterDecision{
				Outcome: outcome, Confidence: .99, Reason: "synthetic-foreign",
			}}}
			client, budget := &auditedFakeLLM{}, &auditBudgetProbe{}
			filter := NewWithLLMAdmission(cfg, client, LLMCallbacks{}, Admission{Budget: budget, Capture: capture})
			decision, err := filter.DecideAudited(context.Background(), Input{Title: "Synthetic Feature 2026"}, enforcementSource(1))
			require.NoError(t, err)
			require.True(t, decision.Allow)
			require.False(t, decision.WouldDrop)
			require.Zero(t, client.calls.Load())
			require.Zero(t, budget.calls)
		})
	}
}

func TestAuditedApplicationRequiresCompleteResultAndDecisionRecording(t *testing.T) {
	for _, tc := range []struct {
		name    string
		capture *auditCaptureProbe
	}{
		{"result write fails", &auditCaptureProbe{resultErr: errors.New("synthetic result write failure")}},
		{"decision write fails", &auditCaptureProbe{decisionErr: errors.New("synthetic decision write failure")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := phase2Config()
			cfg.LLMEnforce = "true"
			client := &enforcementResultClient{
				verdict: LLMVerdict{Confidence: .99, Reason: "synthetic-foreign"},
				result:  llmcapture.HTTPResult{StatusCode: 200, ErrorClass: "none"},
			}
			filter := NewWithLLMAdmission(cfg, client, LLMCallbacks{}, Admission{Budget: &auditBudgetProbe{}, Capture: tc.capture})
			decision, err := filter.DecideAudited(context.Background(), Input{Title: "Synthetic Feature 2026"}, enforcementSource(1))
			require.ErrorIs(t, err, llmcapture.ErrCaptureUnavailable, "missing durable evidence requires retry before application")
			require.False(t, decision.WouldDrop)
			require.False(t, decision.Defer)
		})
	}
}

func TestModelCannotOverrideAdvertisedEnglishReleaseEvidence(t *testing.T) {
	for _, title := range []string{
		"Pelicula.2026.ENG.SPA.1080p",
		"Pelicula 2026 [English Sub]",
		"Pelicula.2026.English.Audio",
		"Pelicula.2026.EN.1080p",
	} {
		t.Run(title, func(t *testing.T) {
			cfg := phase2Config()
			cfg.LLMEnforce = "true"
			client, budget, capture := &auditedFakeLLM{}, &auditBudgetProbe{}, &auditCaptureProbe{}
			filter := NewWithLLMAdmission(cfg, client, LLMCallbacks{}, Admission{Budget: budget, Capture: capture})
			require.False(t, filter.EvaluationLLMEligible(Input{Title: title}), "prospective evaluation must reuse the release evidence veto")
			require.False(t, filter.CaptureLLMEligible(Input{Title: title}))
			decision, err := filter.DecideAudited(context.Background(), Input{Title: title}, enforcementSource(1))
			require.NoError(t, err)
			require.True(t, decision.Allow)
			require.False(t, decision.WouldDrop)
			require.Zero(t, client.calls.Load())
			require.Zero(t, budget.calls)
			require.Zero(t, capture.captures)
		})
	}
}

func TestEnglishReleaseVetoDoesNotBypassDeterministicPolicy(t *testing.T) {
	cfg := phase2Config()
	cfg.Enforce, cfg.LLMEnforce = true, "false"
	decision := New(cfg).DecideDeterministic(Input{Title: "Synthetic.ENG.2026", PrimaryExtension: "exe"})
	require.False(t, decision.Allow)
	require.True(t, decision.WouldDrop)
	require.Equal(t, ReasonBlockedExtension, decision.Reason)
}

func TestProductionAdmissionCannotUseDirectCacheToBypassAudit(t *testing.T) {
	cfg := phase2Config()
	cfg.LLMEnforce = "true"
	client := &fakeLLM{}
	filter := NewWithLLMAdmission(cfg, client, LLMCallbacks{}, Admission{Budget: &auditBudgetProbe{}, Capture: &auditCaptureProbe{}})
	filter.cache.Put(llmCacheKey(cfg.LLMModel, cfg.LLMPromptVersion, normalizeTitle("Synthetic Feature 2026")),
		LLMVerdict{Confidence: 1, Reason: "synthetic-foreign"})
	decision := filter.Decide(Input{Title: "Synthetic Feature 2026"})
	require.True(t, decision.Allow, "legacy Decide must remain a keep even when a matching cache entry exists")
	require.False(t, decision.WouldDrop)
	require.Zero(t, client.calls.Load())
}
