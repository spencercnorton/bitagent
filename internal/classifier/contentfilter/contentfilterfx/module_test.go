package contentfilterfx

import (
	"math"
	"strings"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"go.uber.org/zap"
)

// TestApplyDropLossyAudioOnlyBackCompat pins the legacy-env shim that lets a
// running deploy still set the pre-rename CONTENT_FILTER_DROP_MP_3_ONLY var
// (the old binding for what is now DropLossyAudioOnly) without silently
// changing behavior on upgrade. Precedence: the new env var always wins; the
// legacy var is honored only when the new one is unset.
func TestApplyDropLossyAudioOnlyBackCompat(t *testing.T) {
	logger := zap.NewNop().Sugar()

	cases := []struct {
		name     string
		newEnv   *string // nil = unset
		oldEnv   *string // nil = unset
		startVal bool    // cfg.DropLossyAudioOnly as the resolver left it
		want     bool
	}{
		{
			name:     "neither set keeps resolver value (default true)",
			startVal: true,
			want:     true,
		},
		{
			name:     "legacy false honored when new unset",
			oldEnv:   ptr("false"),
			startVal: true,
			want:     false,
		},
		{
			name:     "new false wins, legacy true ignored",
			newEnv:   ptr("false"),
			oldEnv:   ptr("true"),
			startVal: false, // resolver already applied the new var
			want:     false,
		},
		{
			name:     "legacy unparseable is ignored, resolver value survives",
			oldEnv:   ptr("notabool"),
			startVal: true,
			want:     true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.newEnv != nil {
				t.Setenv("CONTENT_FILTER_DROP_LOSSY_AUDIO_ONLY", *c.newEnv)
			}
			if c.oldEnv != nil {
				t.Setenv("CONTENT_FILTER_DROP_MP_3_ONLY", *c.oldEnv)
			}
			cfg := contentfilter.NewDefaultConfig()
			cfg.DropLossyAudioOnly = c.startVal
			got := applyDropLossyAudioOnlyBackCompat(cfg, logger)
			if got.DropLossyAudioOnly != c.want {
				t.Fatalf("DropLossyAudioOnly = %v, want %v", got.DropLossyAudioOnly, c.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestValidateProductionLLMConfigRequiresBoundedPinnedOpenRouter(t *testing.T) {
	valid := contentfilter.NewDefaultConfig()
	valid.LLMOpenaiApiKey = "test"
	valid.LLMBaseURL = "https://openrouter.ai/api/v1"
	valid.LLMApiStyle = "chat"
	valid.LLMOpenrouterProvider = "azure/us"
	if err := validateProductionLLMConfig(valid); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, mutate := range map[string]func(*contentfilter.Config){
		"missing provider":     func(c *contentfilter.Config) { c.LLMOpenrouterProvider = "" },
		"fallback route":       func(c *contentfilter.Config) { c.LLMBaseURL = "https://example.invalid/v1" },
		"empty route":          func(c *contentfilter.Config) { c.LLMBaseURL = "" },
		"trailing slash route": func(c *contentfilter.Config) { c.LLMBaseURL += "/" },
		"wrong style":          func(c *contentfilter.Config) { c.LLMApiStyle = "responses" },
		"missing key":          func(c *contentfilter.Config) { c.LLMOpenaiApiKey = "" },
		"missing key and route": func(c *contentfilter.Config) {
			c.LLMOpenaiApiKey, c.LLMBaseURL = "", ""
		},
		"zero daily":           func(c *contentfilter.Config) { c.LLMDailyBudget = 0 },
		"monthly below daily":  func(c *contentfilter.Config) { c.LLMMonthlyBudget = c.LLMDailyBudget - 1 },
		"zero concurrency":     func(c *contentfilter.Config) { c.LLMMaxConcurrentCalls = 0 },
		"zero request bound":   func(c *contentfilter.Config) { c.LLMMaxRequestBytes = 0 },
		"zero output bound":    func(c *contentfilter.Config) { c.LLMMaxOutputTokens = 0 },
		"zero confidence":      func(c *contentfilter.Config) { c.LLMMinConfidenceForDrop = 0 },
		"confidence above one": func(c *contentfilter.Config) { c.LLMMinConfidenceForDrop = 1.01 },
		"NaN confidence":       func(c *contentfilter.Config) { c.LLMMinConfidenceForDrop = math.NaN() },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := validateProductionLLMConfig(cfg); err == nil || strings.TrimSpace(err.Error()) == "" {
				t.Fatal("invalid production config was accepted")
			}
		})
	}
}

func TestValidateProductionLLMConfigAcceptsOnlyExactOpenAIDataSharingRoute(t *testing.T) {
	valid := contentfilter.NewDefaultConfig()
	valid.LLMOpenaiApiKey = "test"
	valid.LLMBaseURL = "https://api.openai.com/v1"
	valid.LLMApiStyle = "chat"
	valid.LLMModel = "gpt-5.6-sol"
	valid.LLMOpenaiDataSharing = true
	if err := validateProductionLLMConfig(valid); err != nil {
		t.Fatalf("valid data-sharing config: %v", err)
	}
	for name, mutate := range map[string]func(*contentfilter.Config){
		"wrong model":    func(c *contentfilter.Config) { c.LLMModel = "gpt-6-astra" },
		"wrong endpoint": func(c *contentfilter.Config) { c.LLMBaseURL += "/" },
		"wrong style":    func(c *contentfilter.Config) { c.LLMApiStyle = "responses" },
		"missing key":    func(c *contentfilter.Config) { c.LLMOpenaiApiKey = "" },
		"router pin":     func(c *contentfilter.Config) { c.LLMOpenrouterProvider = "azure/us" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			mutate(&cfg)
			if err := validateProductionLLMConfig(cfg); err == nil {
				t.Fatal("invalid data-sharing route was accepted")
			}
		})
	}
}
