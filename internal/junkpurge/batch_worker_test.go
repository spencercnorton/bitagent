package junkpurge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidateBatchConfig(t *testing.T) {
	t.Run("disabled ignores provider-only settings", func(t *testing.T) {
		cfg := NewDefaultConfig()
		require.NoError(t, validateBatchConfig(cfg))
	})

	valid := NewDefaultConfig()
	valid.LLMBatchEnabled = true
	valid.LLMApiStyle = "chat"
	valid.LLMBaseURL = "https://api.openai.com/v1"
	valid.LLMApiKey = "test-key"

	t.Run("valid", func(t *testing.T) {
		require.NoError(t, validateBatchConfig(valid))
	})

	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"api style", func(c *Config) { c.LLMApiStyle = "ollama" }, "llm_api_style=chat"},
		{"base url", func(c *Config) { c.LLMBaseURL = " " }, "llm_base_url"},
		{"endpoint url", func(c *Config) {
			c.LLMBaseURL = "https://api.openai.com/v1/chat/completions"
		}, "API root"},
		{"official cleartext", func(c *Config) {
			c.LLMBaseURL = "http://api.openai.com/v1"
		}, "requires https"},
		{"official wrong path", func(c *Config) {
			c.LLMBaseURL = "https://api.openai.com"
		}, "must end at /v1"},
		{"query in url", func(c *Config) {
			c.LLMBaseURL = "https://api.openai.com/v1?x=y"
		}, "query"},
		{"official key", func(c *Config) { c.LLMApiKey = "" }, "llm_api_key"},
		{"model", func(c *Config) { c.LLMModel = "" }, "llm_model"},
		{"poll", func(c *Config) { c.LLMBatchPollInterval = 0 }, "poll_interval"},
		{"in flight", func(c *Config) { c.LLMBatchMaxInFlight = 0 }, "max_in_flight"},
		{"attempts", func(c *Config) { c.LLMBatchMaxAttempts = 0 }, "max_attempts"},
		{"window", func(c *Config) { c.LLMBatchCompletionWindow = "1h" }, "must be 24h"},
		{"failure cooldown", func(c *Config) {
			c.LLMBatchFailureCooldown = 0
		}, "failure_cooldown"},
		{"ambiguity grace", func(c *Config) {
			c.LLMBatchAmbiguityGrace = 0
		}, "ambiguity_grace"},
		{"sync fallback", func(c *Config) { c.LLMBatchFallbackSync = true }, "fallback"},
		{"paid sync overlap", func(c *Config) { c.LLMAllowPaidSync = true }, "allow_paid_sync"},
		{"request limit", func(c *Config) { c.BatchSize = 50_001 }, "50000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.edit(&cfg)
			require.ErrorContains(t, validateBatchConfig(cfg), tt.want)
		})
	}
}

func TestValidateBatchConfigRejectsFallbackWhenBatchDisabled(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.LLMBatchFallbackSync = true
	require.ErrorContains(t, validateBatchConfig(cfg), "fallback")
}

func TestAmbiguityGrace(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	recent := now.Add(-30 * time.Minute)
	old := now.Add(-2 * time.Hour)
	require.True(t, ambiguityGraceActive(
		batchAttempt{State: "submitting", SubmissionAttemptedAt: &recent},
		"submitting", time.Hour, now,
	))
	require.False(t, ambiguityGraceActive(
		batchAttempt{State: "submitting", SubmissionAttemptedAt: &old},
		"submitting", time.Hour, now,
	))
	require.False(t, ambiguityGraceActive(
		batchAttempt{State: "uploaded", SubmissionAttemptedAt: &recent},
		"submitting", time.Hour, now,
	))
}

func TestBatchAttemptMetadataIsStable(t *testing.T) {
	attempt := batchAttempt{
		ID:          42,
		RunID:       7,
		AttemptNo:   3,
		InputSHA256: "abc123",
	}
	require.Equal(t, map[string]string{
		"bitagent_component": "junkpurge",
		"run_id":             "7",
		"attempt_id":         "42",
		"attempt_no":         "3",
		"input_sha256":       "abc123",
	}, batchAttemptMetadata(attempt))
}

func TestTerminalBatchStatuses(t *testing.T) {
	for _, status := range []string{"completed", "failed", "expired", "cancelled"} {
		require.True(t, isTerminalBatchStatus(status), status)
	}
	for _, status := range []string{
		"validating", "in_progress", "finalizing", "cancelling", "",
	} {
		require.False(t, isTerminalBatchStatus(status), status)
	}
}

func TestMissingBatchResultRetryPolicy(t *testing.T) {
	require.True(t, missingBatchResultRetryable("expired"))
	require.True(t, batchStatusAllowsItemRetry("expired"))
	require.True(t, batchStatusAllowsItemRetry("completed"))
	for _, status := range []string{"completed", "failed", "cancelled"} {
		require.False(t, missingBatchResultRetryable(status), status)
	}
	for _, status := range []string{"failed", "cancelled"} {
		require.False(t, batchStatusAllowsItemRetry(status), status)
	}
}

func TestValidateBatchConfigRejectsNegativeDuration(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.LLMBatchEnabled = true
	cfg.LLMApiStyle = "chat"
	cfg.LLMBaseURL = "https://api.openai.com/v1"
	cfg.LLMApiKey = "test-key"
	cfg.LLMBatchPollInterval = -time.Second
	require.ErrorContains(t, validateBatchConfig(cfg), "poll_interval")
}
