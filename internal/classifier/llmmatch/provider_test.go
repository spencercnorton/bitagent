package llmmatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenRouterPolicyMatchesCapturedEvaluationBytes(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Endpoint = "https://openrouter.ai/api/v1/chat/completions"
	cfg.Model = "openai/gpt-5.4-nano"
	cfg.OpenrouterProvider = "azure/us"
	c := NewClient(cfg, nil, NewMetrics(), zap.NewNop().Sugar())
	expected, err := EvaluationConfiguredChatRequestJSON(cfg, "policy", "input", 120)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(expected, &object))
	provider := object["provider"].(map[string]any)
	require.Equal(t, map[string]any{
		"order": []any{"azure/us"}, "only": []any{"azure/us"},
		"allow_fallbacks": false, "require_parameters": true, "data_collection": "deny", "zdr": true,
	}, provider)
	called := false
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)
		require.Equal(t, expected, body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{}"}}]}`)), Header: make(http.Header)}, nil
	})
	_, err = c.call(context.Background(), "extract", "policy", "input", 120)
	require.NoError(t, err)
	require.True(t, called)
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
		cfg.Enabled, cfg.Endpoint, cfg.OpenrouterProvider = true, tc.endpoint, tc.provider
		require.Error(t, cfg.Validate())
		_, err := EvaluationConfiguredChatRequestJSON(cfg, "policy", "input", 120)
		require.Error(t, err)
	}
	cfg := NewDefaultConfig()
	cfg.Model = "gpt-5.4-nano"
	raw, err := EvaluationConfiguredChatRequestJSON(cfg, "policy", "input", 120)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"provider"`, "direct API requests must not receive OpenRouter-only parameters")
}

func TestOpenAIDataSharingRequestIsDirectBoundedAndAuditable(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.APIKey = "test"
	cfg.Endpoint = "https://api.openai.com/v1/chat/completions"
	cfg.Model = "gpt-5.6-sol"
	cfg.OpenaiDataSharing = true
	require.NoError(t, cfg.Validate())

	raw, err := EvaluationConfiguredChatRequestJSON(cfg, "policy", "input", 120)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(raw, &object))
	require.Equal(t, "gpt-5.6-sol", object["model"])
	require.Equal(t, "none", object["reasoning_effort"])
	require.Equal(t, false, object["store"])
	require.NotContains(t, object, "provider")
	require.NotContains(t, object["messages"].([]any)[1].(map[string]any)["content"], "/no_think")
	require.Equal(t,
		"llmmatch-chat-extract-v1-openai-data-sharing-v1",
		matcherContractID(cfg, "llmmatch-chat-extract-v1"),
	)
}
