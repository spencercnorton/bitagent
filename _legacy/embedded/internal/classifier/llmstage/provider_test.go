package llmstage

import (
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestOpenRouterPolicyIsPinnedInExactRequest(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.APIKey = "test"
	cfg.Endpoint = "https://openrouter.ai/api/v1/chat/completions"
	cfg.Model = "openai/gpt-5.4-nano-20260317"
	cfg.OpenrouterProvider = "azure/us"
	require.NoError(t, cfg.Validate())
	require.Equal(t, "classifier-type-v2-openrouter", typeContractID(cfg))

	raw := buildBoundedRequestBody(cfg, baseTorrent())
	var object map[string]any
	require.NoError(t, json.Unmarshal(raw, &object))
	provider := object["provider"].(map[string]any)
	require.Equal(t, map[string]any{
		"order": []any{"azure/us"}, "only": []any{"azure/us"},
		"allow_fallbacks": false, "require_parameters": true,
		"data_collection": "deny", "zdr": true,
	}, provider)
	require.NotContains(t, string(raw), `"temperature"`)
	direct := NewDefaultConfig()
	require.NotContains(t, string(buildBoundedRequestBody(direct, baseTorrent())), `"provider"`)
	require.Equal(t, "classifier-type-v1", typeContractID(direct))
}

func TestOpenRouterRejectsMissingPinAndWrongEndpoint(t *testing.T) {
	for _, tc := range []struct{ endpoint, provider string }{
		{"https://openrouter.ai/api/v1/chat/completions", ""},
		{"https://openrouter.ai./api/v1/chat/completions", ""},
		{"https://OPENROUTER.AI./api/v1/chat/completions", ""},
		{"https://openrouter.ai./api/v1/chat/completions", "azure/us"},
		{"http://openrouter.ai/api/v1/chat/completions", "azure/us"},
		{"https://other.example/api/v1/chat/completions", "azure/us"},
		{"https://openrouter.ai/api/v1/responses", "azure/us"},
		{"https://openrouter.ai/api/v1/chat/completions?override=1", "azure/us"},
		{"https://openrouter.ai/api/v1/chat/completions#ignored", "azure/us"},
		{"https://openrouter.ai/api/v1/chat%2Fcompletions", "azure/us"},
	} {
		cfg := NewDefaultConfig()
		cfg.Enabled, cfg.APIKey, cfg.Endpoint, cfg.OpenrouterProvider = true, "test", tc.endpoint, tc.provider
		require.Error(t, cfg.Validate())
	}
}

func TestOpenAIDataSharingTypeRequestIsDirectAndBounded(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.APIKey = "test"
	cfg.Endpoint = "https://api.openai.com/v1/chat/completions"
	cfg.Model = "gpt-5.6-sol"
	cfg.OpenaiDataSharing = true
	require.NoError(t, cfg.Validate())
	require.Equal(t, "classifier-type-v3-openai-data-sharing", typeContractID(cfg))

	raw := buildBoundedRequestBody(cfg, baseTorrent())
	var object map[string]any
	require.NoError(t, json.Unmarshal(raw, &object))
	require.Equal(t, "none", object["reasoning_effort"])
	require.Equal(t, false, object["store"])
	require.NotContains(t, object, "provider")
}

func TestTypeUsageMetricsAreCompleteOrExplicitlyMissing(t *testing.T) {
	cfg := NewDefaultConfig()
	m := NewMetrics()
	s := &Stage{cfg: cfg, metrics: m}
	s.recordUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":5}}}`))
	for kind, expected := range map[string]float64{"input": 100, "output": 20, "cached_input": 40, "reasoning": 5} {
		require.Equal(t, expected, testutil.ToFloat64(m.tokensTotal.WithLabelValues(cfg.Model, kind)))
	}
	s.recordUsage([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`))
	s.recordUsage([]byte(`{}`))
	s.recordUsage([]byte(`{"usage":{"prompt_tokens":"unknown","completion_tokens":0}}`))
	require.Equal(t, float64(3), testutil.ToFloat64(m.usageMissingTotal.WithLabelValues(cfg.Model)))
}
