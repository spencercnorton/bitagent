package junkpurge

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/lazy"
	"gorm.io/gorm"
)

// TestDefaultConfigSafe — a new deploy must do nothing: both Enabled and
// EnablePurge default off, and the thresholds match the agreed policy.
func TestDefaultConfigSafe(t *testing.T) {
	c := NewDefaultConfig()
	if c.Enabled {
		t.Error("NewDefaultConfig must not enable junkpurge by default")
	}
	if c.EnablePurge {
		t.Error("NewDefaultConfig must not enable real deletion by default; dry-run only")
	}
	if c.MinConfidence != 0.8 {
		t.Errorf("MinConfidence = %v, want 0.8", c.MinConfidence)
	}
	if c.MinAge.Hours() != 7*24 {
		t.Errorf("MinAge = %v, want 7d", c.MinAge)
	}
	if c.MaxJunkRate <= 0 || c.MaxJunkRate > 1 {
		t.Errorf("MaxJunkRate = %v, want (0,1]", c.MaxJunkRate)
	}
	if c.QuarantineDays != 30 {
		t.Errorf("QuarantineDays = %d, want 30 (review window)", c.QuarantineDays)
	}
	if c.LLMBatchEnabled {
		t.Error("LLMBatchEnabled must default false; provider Batch requires an explicit rollout")
	}
	if c.LLMBatchPollInterval != time.Minute {
		t.Errorf("LLMBatchPollInterval = %v, want 1m", c.LLMBatchPollInterval)
	}
	if c.LLMBatchMaxInFlight != 1 {
		t.Errorf("LLMBatchMaxInFlight = %d, want 1", c.LLMBatchMaxInFlight)
	}
	if c.LLMBatchMaxAttempts != 3 {
		t.Errorf("LLMBatchMaxAttempts = %d, want 3", c.LLMBatchMaxAttempts)
	}
	if c.LLMBatchCompletionWindow != "24h" {
		t.Errorf("LLMBatchCompletionWindow = %q, want 24h", c.LLMBatchCompletionWindow)
	}
	if c.LLMBatchFailureCooldown != 24*time.Hour {
		t.Errorf("LLMBatchFailureCooldown = %v, want 24h", c.LLMBatchFailureCooldown)
	}
	if c.LLMBatchAmbiguityGrace != time.Hour {
		t.Errorf("LLMBatchAmbiguityGrace = %v, want 1h", c.LLMBatchAmbiguityGrace)
	}
	if c.LLMBatchFallbackSync {
		t.Error("LLMBatchFallbackSync must default false; Batch must never silently fall back")
	}
	if c.LLMAllowPaidSync {
		t.Error("LLMAllowPaidSync must default false; hosted sync spend requires opt-in")
	}
	if c.LLMDailyCallLimit != 50 || c.LLMMonthlyCallLimit != 1500 {
		t.Errorf(
			"LLM call limits = %d/%d, want 50/1500",
			c.LLMDailyCallLimit, c.LLMMonthlyCallLimit,
		)
	}
	if c.LLMUnavailableConsecutiveLimit != 3 {
		t.Errorf(
			"LLMUnavailableConsecutiveLimit = %d, want 3",
			c.LLMUnavailableConsecutiveLimit,
		)
	}
}

func TestAllowStandardLLMRequiresHostedSpendOptIn(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.LLMApiStyle = "chat"
	if allowStandardLLM(cfg) {
		t.Fatal("chat-compatible sync must default off")
	}
	cfg.LLMAllowPaidSync = true
	if !allowStandardLLM(cfg) {
		t.Fatal("explicit paid-sync opt-in must enable standard mode")
	}
	cfg.LLMApiStyle = "ollama"
	cfg.LLMAllowPaidSync = false
	if !allowStandardLLM(cfg) {
		t.Fatal("native Ollama sync should not require a paid-spend opt-in")
	}
	cfg.LLMApiStyle = "openai"
	if allowStandardLLM(cfg) {
		t.Fatal("unknown API styles must never bypass the paid-sync gate")
	}
	cfg.Enabled = true
	if err := validateWorkerConfig(cfg); err == nil {
		t.Fatal("unknown API style must fail startup validation")
	}
}

func TestOpenAIDataSharingSyncRouteIsStrictAndBounded(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.LLMBaseURL = "https://api.openai.com/v1"
	cfg.LLMApiStyle = "chat"
	cfg.LLMModel = "gpt-5.6-sol"
	cfg.LLMApiKey = "test"
	cfg.LLMOpenaiDataSharing = true
	cfg.LLMAllowPaidSync = true
	if err := validateWorkerConfig(cfg); err != nil {
		t.Fatalf("valid data-sharing config: %v", err)
	}
	body := buildChatRequestForRoute(cfg.LLMModel, "example", true)
	if body["reasoning_effort"] != "none" || body["store"] != false {
		t.Fatalf("data-sharing controls missing: %#v", body)
	}
	if strings.Contains(body["messages"].([]map[string]string)[0]["content"], "/no_think") {
		t.Fatal("direct OpenAI request retained Ollama-only prompt hint")
	}

	cfg.LLMBatchEnabled = true
	cfg.LLMAllowPaidSync = false
	if err := validateWorkerConfig(cfg); err == nil {
		t.Fatal("Batch was accepted without confirmed incentive eligibility")
	}
}

func TestHostedSyncRequiresPositiveDurableLimits(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.LLMDailyCallLimit = 0 },
		func(c *Config) { c.LLMMonthlyCallLimit = 0 },
	} {
		cfg := NewDefaultConfig()
		cfg.Enabled = true
		cfg.LLMApiStyle = "chat"
		cfg.LLMAllowPaidSync = true
		mutate(&cfg)
		if err := validateWorkerConfig(cfg); err == nil {
			t.Fatal("hosted sync accepted without a positive durable limit")
		}
	}
}

func TestCaptureOnlyRequiresExplicitCaptureAndKeepsPaidPathsOff(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.LLMApiStyle = "chat"
	capture := &junkCaptureFake{}

	if !allowCaptureOnly(cfg, capture) {
		t.Fatal("enabled capture should admit observation-only junk collection")
	}
	cfg.LLMAllowPaidSync = true
	if allowCaptureOnly(cfg, capture) {
		t.Fatal("paid synchronous mode must not be mislabeled capture-only")
	}
	cfg.LLMAllowPaidSync = false
	cfg.LLMBatchEnabled = true
	if allowCaptureOnly(cfg, capture) {
		t.Fatal("Batch mode must not be mislabeled capture-only")
	}
	cfg.LLMBatchEnabled = false
	if allowCaptureOnly(cfg, nil) {
		t.Fatal("missing capture recorder must remain drain-only")
	}
}

func TestCaptureOnlyRemainsObservationOnlyWhenEnablePurgeIsStale(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnablePurge = true
	cfg.LLMApiStyle = "chat"
	cfg.LLMBatchEnabled = false
	cfg.LLMAllowPaidSync = false

	if !allowCaptureOnly(cfg, &junkCaptureFake{}) {
		t.Fatal("capture-only mode must remain identifiable with EnablePurge=true")
	}
	if allowDestructiveExpiry(cfg, &junkCaptureFake{}) {
		t.Fatal("capture-only mode must suppress destructive quarantine expiry")
	}
}

func TestCaptureOnlyProcessingPlanStartsOnlyObservationLoop(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.EnablePurge = true
	cfg.LLMApiStyle = "chat"
	cfg.LLMBatchEnabled = false
	cfg.LLMAllowPaidSync = false

	plan := processingPlan(cfg, &junkCaptureFake{})
	if plan.name != "capture-only" {
		t.Fatalf("processing plan = %q, want capture-only", plan.name)
	}
	if !plan.startSyncLoop {
		t.Fatal("capture-only must start its observation loop")
	}
	if plan.startBatchLoop || plan.admitNewBatch || plan.allowBatchPurge {
		t.Fatalf("capture-only must make no Batch/provider plan: %+v", plan)
	}
	if !plan.requireBatchIdle {
		t.Fatal("capture-only must fail startup unless durable Batch work is idle")
	}
}

func TestWorkerModeReflectsActualAdmissionPath(t *testing.T) {
	cfg := NewDefaultConfig()
	if got := workerMode(cfg); got != "DRAIN-ONLY" {
		t.Fatalf("disabled mode = %q, want DRAIN-ONLY", got)
	}

	cfg.Enabled = true
	cfg.LLMApiStyle = "chat"
	cfg.EnablePurge = true
	if got := workerMode(cfg); got != "DRAIN-ONLY" {
		t.Fatalf("paid-sync-fused mode = %q, want DRAIN-ONLY", got)
	}

	cfg.LLMBatchEnabled = true
	if got := workerMode(cfg); got != "LIVE-DELETE" {
		t.Fatalf("live Batch mode = %q, want LIVE-DELETE", got)
	}

	cfg.EnablePurge = false
	if got := workerMode(cfg); got != "DRY-RUN" {
		t.Fatalf("Batch canary mode = %q, want DRY-RUN", got)
	}
}

func TestWorkerStartFailsWhenMigratedDatabaseInitializationFails(t *testing.T) {
	wantErr := errors.New("migration failed")
	w := &purgeWorker{
		cfg: NewDefaultConfig(),
		gormDB: lazy.New(func() (*gorm.DB, error) {
			return nil, wantErr
		}),
	}

	err := w.start(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("start error = %v, want wrapped %v", err, wantErr)
	}
	if w.cancel != nil {
		t.Fatal("worker goroutines must not start before migrations succeed")
	}
}

func TestValidateWorkerConfigRejectsInvalidJunkRate(t *testing.T) {
	for _, value := range []float64{
		-0.1,
		0,
		1.1,
		math.NaN(),
		math.Inf(1),
	} {
		cfg := NewDefaultConfig()
		cfg.Enabled = true
		cfg.MaxJunkRate = value
		if err := validateWorkerConfig(cfg); err == nil {
			t.Errorf("MaxJunkRate=%v: expected validation error", value)
		}
	}

	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.MaxJunkRate = 1
	if err := validateWorkerConfig(cfg); err != nil {
		t.Fatalf("MaxJunkRate=1 should be valid: %v", err)
	}
}

func TestValidateWorkerConfigRejectsInvalidConfidence(t *testing.T) {
	for _, value := range []float64{
		-0.1,
		0,
		1.1,
		math.NaN(),
		math.Inf(1),
	} {
		cfg := NewDefaultConfig()
		cfg.Enabled = true
		cfg.MinConfidence = value
		if err := validateWorkerConfig(cfg); err == nil {
			t.Errorf("MinConfidence=%v: expected validation error", value)
		}
	}
}

func TestValidateWorkerConfigRejectsInvalidUnavailableGroupLimit(t *testing.T) {
	for _, value := range []int{-1, 0, 11} {
		cfg := NewDefaultConfig()
		cfg.Enabled = true
		cfg.LLMUnavailableConsecutiveLimit = value
		if err := validateWorkerConfig(cfg); err == nil {
			t.Errorf(
				"LLMUnavailableConsecutiveLimit=%d: expected validation error",
				value,
			)
		}
	}
}

func TestIsJunk(t *testing.T) {
	cases := []struct {
		j    Judgment
		min  float64
		want bool
	}{
		{Judgment{verdictJunk, 0.9}, 0.8, true},
		{Judgment{verdictJunk, 0.8}, 0.8, true},   // boundary inclusive
		{Judgment{verdictJunk, 0.79}, 0.8, false}, // below floor
		{Judgment{verdictRealMangled, 0.99}, 0.8, false},
		{Judgment{verdictRealAbsent, 0.99}, 0.8, false},
		{Judgment{verdictUnsure, 1.0}, 0.8, false},
	}
	for _, c := range cases {
		if got := c.j.IsJunk(c.min); got != c.want {
			t.Errorf("IsJunk(%+v, %v) = %v, want %v", c.j, c.min, got, c.want)
		}
	}
}

func TestParseJudgment(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantVerdict string
		wantConf    float64
		wantErr     bool
	}{
		{"clean", `{"verdict":"junk","confidence":0.9}`, verdictJunk, 0.9, false},
		{"think-wrapped", "<think>hmm this is real</think>\n{\"verdict\":\"real_mangled\",\"confidence\":0.7}", verdictRealMangled, 0.7, false},
		{"fenced", "```json\n{\"verdict\":\"real_absent\",\"confidence\":0.6}\n```", verdictRealAbsent, 0.6, false},
		{"unknown-verdict", `{"verdict":"definitely_junk","confidence":0.95}`, "", 0, true},
		{"confidence-too-high", `{"verdict":"junk","confidence":1.7}`, "", 0, true},
		{"confidence-too-low", `{"verdict":"junk","confidence":-0.2}`, "", 0, true},
		{"missing-verdict", `{"confidence":0.95}`, "", 0, true},
		{"null-verdict", `{"verdict":null,"confidence":0.95}`, "", 0, true},
		{"missing-confidence", `{"verdict":"junk"}`, "", 0, true},
		{"null-confidence", `{"verdict":"junk","confidence":null}`, "", 0, true},
		{"extra-field", `{"verdict":"junk","confidence":0.95,"extra":true}`, "", 0, true},
		{"top-level-null", "null", "", 0, true},
		{"empty", "", "", 0, true},
		{"garbage", "not json at all", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j, err := parseJudgment(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if j.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", j.Verdict, c.wantVerdict)
			}
			if j.Confidence != c.wantConf {
				t.Errorf("confidence = %v, want %v", j.Confidence, c.wantConf)
			}
		})
	}
}

// newOllamaServer returns an httptest server speaking Ollama's /api/chat
// shape, returning the given status + message content.
func newOllamaServer(t *testing.T, status int, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %q (want /api/chat)", r.URL.Path)
		}
		w.WriteHeader(status)
		if status/100 == 2 {
			_, _ = w.Write([]byte(`{"message":{"content":` + jsonQuote(content) + `}}`))
		} else {
			_, _ = w.Write([]byte(`upstream error`))
		}
	}))
}

func jsonQuote(s string) string {
	// minimal JSON string-escaping for test fixtures
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		default:
			out = append(out, string(r)...)
		}
	}
	return string(append(out, '"'))
}

func testJudge(url string) Judge {
	return NewJudge(
		Config{LLMBaseURL: url + "/v1", LLMModel: "m", LLMApiStyle: "ollama", LLMTimeout: "5s"},
		NewMetrics(),
	)
}

func TestJudgeOllamaParsesVerdict(t *testing.T) {
	srv := newOllamaServer(t, 200, `{"verdict":"junk","confidence":0.92}`)
	defer srv.Close()
	j, err := testJudge(srv.URL).Judge(context.Background(), "some.fake.keygen.exe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !j.IsJunk(0.8) {
		t.Errorf("expected confident junk, got %+v", j)
	}
}

// TestJudge5xxIsUnavailable — a 5xx must classify as ErrLLMUnavailable so the
// worker's circuit breaker trips (skip deletion) rather than treating it as a
// per-item miss.
func TestJudge5xxIsUnavailable(t *testing.T) {
	srv := newOllamaServer(t, 503, "")
	defer srv.Close()
	_, err := testJudge(srv.URL).Judge(context.Background(), "anything")
	if !errors.Is(err, ErrLLMUnavailable) {
		t.Errorf("503 must be ErrLLMUnavailable, got %v", err)
	}
}

// TestJudge4xxNotUnavailable — a 4xx is a bad request, not an outage: it must
// NOT be ErrLLMUnavailable (so it stays a per-item skip, not a breaker trip).
func TestJudge4xxNotUnavailable(t *testing.T) {
	srv := newOllamaServer(t, 400, "")
	defer srv.Close()
	_, err := testJudge(srv.URL).Judge(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if errors.Is(err, ErrLLMUnavailable) {
		t.Errorf("400 must NOT be ErrLLMUnavailable, got %v", err)
	}
}

// a non-positive cooldown restores the unbounded per-item retry the
// cooldown exists to stop, so it must fail startup rather than quietly degrade
// back to re-billing a permanently-bad reply every cycle.
func TestValidateWorkerConfigRejectsNonPositiveFailureCooldown(t *testing.T) {
	for _, value := range []time.Duration{-time.Hour, 0} {
		cfg := NewDefaultConfig()
		cfg.Enabled = true
		cfg.LLMFailureCooldown = value
		if err := validateWorkerConfig(cfg); err == nil {
			t.Errorf("LLMFailureCooldown=%v: expected validation error", value)
		}
	}

	cfg := NewDefaultConfig()
	cfg.Enabled = true
	if cfg.LLMFailureCooldown <= 0 {
		t.Fatalf("the default must be positive, got %v", cfg.LLMFailureCooldown)
	}
	if err := validateWorkerConfig(cfg); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
}
