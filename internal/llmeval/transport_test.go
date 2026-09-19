package llmeval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
	"github.com/stretchr/testify/require"
)

func TestHostedClientPinsOpenRouterAndUsesStrictSchema(t *testing.T) {
	var requestBody map[string]any
	temperature := 0.0
	seed := int64(20260724)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.Equal(t, "enabled", r.Header.Get("X-OpenRouter-Metadata"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"generation-1","model":"mistralai/mistral-nemo","provider":"Undocumented Wrong Value",
			"choices":[{"message":{"content":"{\"verdict\":\"real_mangled\",\"confidence\":0.98}"}}],
			"usage":{"prompt_tokens":100,"completion_tokens":10,"cost":0.0000021},
			"openrouter_metadata":{"endpoints":{"available":[
				{"provider":"DekaLLM","model":"mistralai/mistral-nemo","selected":true}
			]}}
		}`))
	}))
	defer server.Close()

	client := &HostedClient{HTTP: server.Client()}
	system := SystemConfig{
		SystemID: "nemo", Provider: "openrouter", Model: "mistralai/mistral-nemo",
		PromptVersion: "v2", EvaluationLane: EvaluationLaneNormalizedStrict,
		APIKind: APIKindChat, OutputContract: OutputContractJSONSchema, BaseURL: server.URL,
		ProviderEndpoint: "dekallm/fp8", Tasks: []Task{TaskJunkPurge},
		InputUSDPerMillion: 0.018, OutputUSDPerMillion: 0.03,
		BillingMultiplier: 1.055, StructuredOutputs: true, ZDR: true,
		NoThinkLocation: NoThinkLocationNone, UseMaxTokens: true,
		Temperature: &temperature, Seed: &seed,
	}
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskJunkPurge, SchemaName: "junk", System: "policy", User: "name",
			MaxCompletionTokens: 120, Schema: junkPurgeSchema(),
		},
	)
	require.NoError(t, err)
	require.JSONEq(t, `{"verdict":"real_mangled","confidence":0.98}`, string(completion.Text))
	require.Equal(t, int64(2), completion.Usage.CostMicroUSD)
	require.Equal(t, CostSourceProviderReported, completion.Usage.CostSource)
	require.Equal(t, "generation-1", completion.Usage.RequestID)

	provider := requestBody["provider"].(map[string]any)
	require.Equal(t, false, provider["allow_fallbacks"])
	require.Equal(t, true, provider["require_parameters"])
	require.Equal(t, "deny", provider["data_collection"])
	require.Equal(t, true, provider["zdr"])
	require.Equal(t, []any{"dekallm"}, provider["order"])
	require.Equal(t, []any{"dekallm"}, provider["only"])
	require.Equal(t, []any{"fp8"}, provider["quantizations"])
	responseFormat := requestBody["response_format"].(map[string]any)
	require.Equal(t, "json_schema", responseFormat["type"])
	require.Equal(t, float64(120), requestBody["max_tokens"])
	require.NotContains(t, requestBody, "max_completion_tokens")
	require.Equal(t, float64(0), requestBody["temperature"])
	require.Equal(t, float64(20260724), requestBody["seed"])
}

func TestHostedClientReturnsUsageWithEmptyContentError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "enabled", r.Header.Get("X-OpenRouter-Metadata"))
		_, _ = w.Write([]byte(`{
			"id":"charged-empty","model":"test/model","provider":"Test Provider",
			"choices":[{"message":{"content":""}}],
			"usage":{"prompt_tokens":25,"completion_tokens":10,"cost":0.000016},
			"openrouter_metadata":{"endpoints":{"available":[
				{"provider":"Test Provider","model":"test/model","selected":true}
			]}}
		}`))
	}))
	defer server.Close()

	client := &HostedClient{HTTP: server.Client()}
	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskContentFilter, SchemaName: "content", System: "policy", User: "name",
			MaxCompletionTokens: 120, Schema: contentFilterSchema(),
		},
	)
	require.EqualError(t, err, "empty_content")
	require.Equal(t, "charged-empty", completion.Usage.RequestID)
	require.Equal(t, int64(17), completion.Usage.CostMicroUSD)
	require.Equal(t, "test/model", completion.ReturnedModel)
	require.Equal(t, "Test Provider", completion.ReturnedProvider)
}

func TestHostedClientReconcilesOpenRouterCacheHitByResponseGenerationID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			require.Equal(t, "cache-generation-1", r.URL.Query().Get("id"))
			require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"data":{
				"id":"cache-generation-1",
				"model":"test/model",
				"provider_name":"Test Provider",
				"native_tokens_prompt":11,
				"native_tokens_completion":4,
				"total_cost":0.000015
			}}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"cache-generation-1","model":"test/model",
			"choices":[{"message":{"content":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"clear\"}"}}],
			"usage":{"prompt_tokens":11,"completion_tokens":4,"cost":0.000015}
		}`))
	}))
	defer server.Close()

	client := &HostedClient{HTTP: server.Client()}
	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskContentFilter, SchemaName: "content", System: "policy", User: "name",
			MaxCompletionTokens: 120, Schema: contentFilterSchema(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, "cache-generation-1", completion.GenerationID)
	require.Equal(t, RouteProofGenerationMetadata, completion.RouteProof)
	require.Equal(t, "test/model", completion.ReturnedModel)
	require.Equal(t, "Test Provider", completion.ReturnedProvider)
	require.Equal(t, int64(16), completion.Usage.CostMicroUSD)
	require.Equal(t, CostSourceProviderReported, completion.Usage.CostSource)
}

func TestHostedClientReconcilesPaidInlineZeroCost(t *testing.T) {
	var generationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			generationCalls.Add(1)
			require.Equal(t, "paid-zero-inline", r.URL.Query().Get("id"))
			_, _ = w.Write([]byte(`{"data":{
				"id":"paid-zero-inline",
				"model":"test/model",
				"provider_name":"Test Provider",
				"native_tokens_prompt":25,
				"native_tokens_completion":5,
				"total_cost":0.00002
			}}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"paid-zero-inline","model":"test/model",
			"choices":[{"message":{"content":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"clear\"}"}}],
			"usage":{"prompt_tokens":25,"completion_tokens":5,"cost":0},
			"openrouter_metadata":{"endpoints":{"available":[
				{"provider":"Test Provider","model":"test/model","selected":true}
			]}}
		}`))
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	completion, err := (&HostedClient{HTTP: server.Client()}).Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskContentFilter, SchemaName: "content", System: "policy", User: "name",
			MaxCompletionTokens: 120, Schema: contentFilterSchema(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, int32(1), generationCalls.Load())
	require.Equal(t, RouteProofGenerationMetadata, completion.RouteProof)
	require.True(t, completion.UsageCostReported)
	require.Equal(t, int64(21), completion.Usage.CostMicroUSD)
}

func TestHostedClientAcceptsInlineZeroCostForFreeRoute(t *testing.T) {
	var generationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			generationCalls.Add(1)
			http.Error(w, "generation lookup must not run", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"free-inline","model":"test/model",
			"choices":[{"message":{"content":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"clear\"}"}}],
			"usage":{"prompt_tokens":25,"completion_tokens":5,"cost":0},
			"openrouter_metadata":{"endpoints":{"available":[
				{"provider":"Test Provider","model":"test/model","selected":true}
			]}}
		}`))
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	system.InputUSDPerMillion = 0
	system.OutputUSDPerMillion = 0
	completion, err := (&HostedClient{HTTP: server.Client()}).Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskContentFilter, SchemaName: "content", System: "policy", User: "name",
			MaxCompletionTokens: 120, Schema: contentFilterSchema(),
		},
	)
	require.NoError(t, err)
	require.Zero(t, generationCalls.Load())
	require.Equal(t, RouteProofRouterMetadata, completion.RouteProof)
	require.True(t, completion.UsageCostReported)
	require.Zero(t, completion.Usage.CostMicroUSD)
}

func TestHostedClientRejectsAuthenticatedZeroCostForPaidRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":{
				"id":"paid-zero-authenticated","model":"test/model",
				"provider_name":"Test Provider","native_tokens_prompt":25,
				"native_tokens_completion":5,"total_cost":0
			}}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"paid-zero-authenticated","model":"test/model",
			"choices":[{"message":{"content":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"clear\"}"}}],
			"usage":{"prompt_tokens":25,"completion_tokens":5,"cost":0},
			"openrouter_metadata":{"endpoints":{"available":[
				{"provider":"Test Provider","model":"test/model","selected":true}
			]}}
		}`))
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	completion, err := (&HostedClient{HTTP: server.Client()}).Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskContentFilter, SchemaName: "content", System: "policy", User: "name",
			MaxCompletionTokens: 120, Schema: contentFilterSchema(),
		},
	)
	require.EqualError(t, err, "route_unverifiable")
	require.Positive(t, completion.Usage.CostMicroUSD,
		"inline estimated cost must not become a forged free observation")
}

func TestHostedClientPreservesAuthenticatedCostWhenTokenShapeIsInvalid(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":{
				"id":"invalid-token-shape","model":"test/model",
				"provider_name":"Test Provider","native_tokens_prompt":25,
				"native_tokens_completion":5,"native_tokens_reasoning":6,
				"total_cost":0.01
			}}`))
			return
		}
		w.Header().Set("X-Generation-Id", "invalid-token-shape")
		_, _ = w.Write([]byte(`{
			"id":"invalid-token-shape","model":"test/model",
			"choices":[{"message":{"content":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"clear\"}"}}],
			"usage":{"prompt_tokens":25,"completion_tokens":5},
			"openrouter_metadata":{"endpoints":{"available":[
				{"provider":"Test Provider","model":"test/model","selected":true}
			]}}
		}`))
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	completion, err := (&HostedClient{HTTP: server.Client()}).Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskContentFilter, SchemaName: "content", System: "policy", User: "name",
			MaxCompletionTokens: 120, Schema: contentFilterSchema(),
		},
	)
	require.NoError(t, err)
	require.False(t, completion.Usage.AccountingComplete)
	require.Zero(t, completion.Usage.InputTokens)
	require.Zero(t, completion.Usage.OutputTokens)
	require.Equal(t, int64(10_550), completion.Usage.CostMicroUSD)
	require.False(t, completion.UsageCostReported)
}

func TestHostedClientPreservesMatcherProductionRequestShape(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Empty(t, r.Header.Get("X-OpenRouter-Metadata"))
		require.Empty(t, r.Header.Get("HTTP-Referer"))
		require.Empty(t, r.Header.Get("X-OpenRouter-Title"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
		_, _ = w.Write([]byte(`{
			"id":"chat-prod","model":"gpt-5.4-nano",
			"choices":[{"message":{"content":"{\"title\":\"Example\",\"year\":2024,\"type\":\"movie\",\"season\":0,\"episode\":0,\"is_anime\":false,\"english\":\"unknown\",\"is_pack\":false,\"is_adult\":false}"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5}
		}`))
	}))
	defer server.Close()

	client := &HostedClient{HTTP: server.Client()}
	system := SystemConfig{
		SystemID: "matcher-production", Provider: "openai", Model: "gpt-5.4-nano",
		PromptVersion: "production", EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind: APIKindChat, OutputContract: OutputContractJSONObject, BaseURL: server.URL,
		Tasks: []Task{TaskMatcherExtract}, NoThinkLocation: NoThinkLocationUser,
		NoThinkFormat: NoThinkFormatDoubleNewline, AppendNoThink: true,
	}
	_, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskMatcherExtract, SchemaName: "extract",
			System: llmmatch.ExtractPrompt(), User: "name",
			MaxCompletionTokens: 120, Schema: matcherExtractSchema(),
		},
	)
	require.NoError(t, err)

	messages := requestBody["messages"].([]any)
	require.Equal(t, llmmatch.ExtractPrompt(), messages[0].(map[string]any)["content"])
	require.Equal(t, "name\n\n/no_think", messages[1].(map[string]any)["content"])
	require.Equal(t, float64(120), requestBody["max_completion_tokens"])
	require.Equal(t, map[string]any{"type": "json_object"}, requestBody["response_format"])
	require.NotContains(t, requestBody, "stream")
	require.NotContains(t, requestBody, "temperature")
	require.NotContains(t, requestBody, "seed")
}

func TestHostedClientShadowFidelityPinsMatcherProductionWire(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.Equal(t, "enabled", r.Header.Get("X-OpenRouter-Metadata"))
		require.Empty(t, r.Header.Get("HTTP-Referer"))
		require.Empty(t, r.Header.Get("X-OpenRouter-Title"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
		_, _ = w.Write([]byte(`{
			"id":"shadow-generation-1","model":"test/open-model",
			"choices":[{"message":{"content":"{\"title\":\"Example\",\"year\":2024,\"type\":\"movie\",\"season\":0,\"episode\":0,\"is_anime\":false,\"english\":\"unknown\",\"is_pack\":false,\"is_adult\":false}"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.000002},
			"openrouter_metadata":{"endpoints":{"available":[
				{"provider":"DeepInfra","model":"test/open-model","selected":true}
			]}}
		}`))
	}))
	defer server.Close()

	system := testShadowFidelitySystem(TaskMatcherExtract)
	system.BaseURL = server.URL
	completion, err := (&HostedClient{HTTP: server.Client()}).Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskMatcherExtract, SchemaName: "extract",
			System: llmmatch.ExtractPrompt(), User: "name",
			MaxCompletionTokens: 120, Schema: matcherExtractSchema(),
		},
	)
	require.NoError(t, err)
	require.Contains(t, string(completion.Text), `"title":"Example"`)
	require.Equal(t, "shadow-generation-1", completion.GenerationID)
	require.Equal(t, "shadow-generation-1", completion.Usage.RequestID)
	require.Equal(t, "test/open-model", completion.ReturnedModel)
	require.Equal(t, "DeepInfra", completion.ReturnedProvider)
	require.Equal(t, RouteProofRouterMetadata, completion.RouteProof)
	require.True(t, completion.UsageCostReported)

	provider := requestBody["provider"].(map[string]any)
	require.Equal(t, []any{"deepinfra"}, provider["order"])
	require.Equal(t, []any{"deepinfra"}, provider["only"])
	require.Equal(t, false, provider["allow_fallbacks"])
	require.Equal(t, true, provider["require_parameters"])
	require.Equal(t, "deny", provider["data_collection"])
	require.Equal(t, true, provider["zdr"])
	require.Equal(t, []any{"fp8"}, provider["quantizations"])
	messages := requestBody["messages"].([]any)
	require.Equal(t, llmmatch.ExtractPrompt(), messages[0].(map[string]any)["content"])
	require.Equal(t, "name\n\n/no_think", messages[1].(map[string]any)["content"])
	require.NotContains(t, requestBody, "stream")
	require.NotContains(t, requestBody, "temperature")
	require.NotContains(t, requestBody, "seed")
}

func TestHostedClientShadowFidelityUsesExactMatcherHTTPBoundary(t *testing.T) {
	valid := `{"id":"shadow-http-boundary","choices":[{"message":{"content":"{}"}}]}`
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "other 2xx is rejected",
			status: http.StatusCreated,
			body:   valid,
			want:   "http_status",
		},
		{
			name:   "live response limit applies",
			status: http.StatusOK,
			body: `{"choices":[{"message":{"content":"` +
				strings.Repeat("x", 129<<10) + `"}}]}`,
			want: "read_response",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			system := testShadowFidelitySystem(TaskMatcherExtract)
			system.BaseURL = server.URL
			_, err := (&HostedClient{HTTP: server.Client()}).Complete(
				context.Background(),
				system,
				"test-key",
				PromptRequest{
					Task: TaskMatcherExtract, System: llmmatch.ExtractPrompt(),
					User: "name", MaxCompletionTokens: 120,
				},
			)
			require.EqualError(t, err, test.want)
		})
	}
}

func TestHostedClientMatcherProductionResponseBoundary(t *testing.T) {
	validContent := `{"title":"Example","year":2024,"type":"movie","season":0,"episode":0,"is_anime":false,"english":"unknown","is_pack":false,"is_adult":false}`
	chatResponse := func(content, refusal string, usage ...any) []byte {
		envelope := map[string]any{
			"id":    "chat-prod",
			"model": "gpt-5.4-nano-2026-07-15",
			"choices": []map[string]any{{
				"message": map[string]string{
					"content": content,
					"refusal": refusal,
				},
			}},
		}
		if len(usage) > 0 {
			envelope["usage"] = usage[0]
		}
		body, err := json.Marshal(envelope)
		require.NoError(t, err)
		return body
	}
	malformedModelResponse := func() []byte {
		body, err := json.Marshal(map[string]any{
			"id":    "chat-prod",
			"model": map[string]string{"unexpected": "shape"},
			"choices": []map[string]any{{
				"message": map[string]string{"content": validContent},
			}},
		})
		require.NoError(t, err)
		return body
	}
	invalidIdentifierResponse := func() []byte {
		body, err := json.Marshal(map[string]any{
			"id":    strings.Repeat("x", maxIDLength+1),
			"model": "gpt-5.4-nano-2026-07-15",
			"choices": []map[string]any{{
				"message": map[string]string{"content": validContent},
			}},
			"usage": map[string]any{
				"prompt_tokens": 10, "completion_tokens": 5,
			},
		})
		require.NoError(t, err)
		return body
	}
	tests := []struct {
		name              string
		status            int
		body              []byte
		responseRequestID string
		wantText          string
		wantErr           string
		wantModel         string
		wantProof         RouteProof
		wantRouteError    string
		wantAccounting    bool
		checkRequestID    bool
		wantRequestID     string
	}{
		{
			name:    "other 2xx rejected",
			status:  http.StatusCreated,
			body:    chatResponse(validContent, ""),
			wantErr: "http_status",
		},
		{
			name:           "refusal with content follows live content path",
			status:         http.StatusOK,
			body:           chatResponse(validContent, "policy"),
			wantText:       validContent,
			wantModel:      "gpt-5.4-nano-2026-07-15",
			wantProof:      RouteProofDirectResponseModel,
			checkRequestID: true,
			wantRequestID:  "chat-prod",
		},
		{
			name:           "empty content reaches model decoder",
			status:         http.StatusOK,
			body:           chatResponse("", ""),
			wantText:       "",
			wantModel:      "gpt-5.4-nano-2026-07-15",
			wantProof:      RouteProofDirectResponseModel,
			checkRequestID: true,
			wantRequestID:  "chat-prod",
		},
		{
			name:   "complete usage is explicitly attested",
			status: http.StatusOK,
			body: chatResponse(validContent, "", map[string]any{
				"prompt_tokens": 10, "completion_tokens": 5,
			}),
			wantText:       validContent,
			wantModel:      "gpt-5.4-nano-2026-07-15",
			wantProof:      RouteProofDirectResponseModel,
			wantAccounting: true,
			checkRequestID: true,
			wantRequestID:  "chat-prod",
		},
		{
			name:           "malformed evaluator-only usage cannot veto live-valid content",
			status:         http.StatusOK,
			body:           chatResponse(validContent, "", map[string]any{"prompt_tokens": "10"}),
			wantText:       validContent,
			wantModel:      "gpt-5.4-nano-2026-07-15",
			wantProof:      RouteProofDirectResponseModel,
			checkRequestID: true,
			wantRequestID:  "chat-prod",
		},
		{
			name:   "impossible auxiliary token partition cannot veto live content",
			status: http.StatusOK,
			body: chatResponse(validContent, "", map[string]any{
				"prompt_tokens": 1, "completion_tokens": 1,
				"prompt_tokens_details": map[string]any{"cached_tokens": 2},
			}),
			wantText:       validContent,
			wantModel:      "gpt-5.4-nano-2026-07-15",
			wantProof:      RouteProofDirectResponseModel,
			checkRequestID: true,
			wantRequestID:  "chat-prod",
		},
		{
			name:              "invalid auxiliary identifiers cannot veto live content",
			status:            http.StatusOK,
			body:              invalidIdentifierResponse(),
			responseRequestID: strings.Repeat("y", maxIDLength+1),
			wantText:          validContent,
			wantModel:         "gpt-5.4-nano-2026-07-15",
			wantProof:         RouteProofDirectResponseModel,
			wantAccounting:    true,
			checkRequestID:    true,
			wantRequestID:     "",
		},
		{
			name:           "malformed evaluator-only model remains unverified",
			status:         http.StatusOK,
			body:           malformedModelResponse(),
			wantText:       validContent,
			wantProof:      RouteProofUnverified,
			wantRouteError: "route_unverifiable",
		},
		{
			name:    "live response limit rejects oversized envelope",
			status:  http.StatusOK,
			body:    chatResponse(strings.Repeat("x", 129<<10), ""),
			wantErr: "read_response",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Empty(t, r.Header.Get("HTTP-Referer"))
				require.Empty(t, r.Header.Get("X-OpenRouter-Title"))
				if test.responseRequestID != "" {
					w.Header().Set("X-Request-Id", test.responseRequestID)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body)
			}))
			defer server.Close()

			client := &HostedClient{HTTP: server.Client()}
			system := SystemConfig{
				SystemID: "matcher-production", Provider: "openai", Model: "gpt-5.4-nano",
				PromptVersion: "production", EvaluationLane: EvaluationLaneProductionFidelity,
				APIKind: APIKindChat, OutputContract: OutputContractJSONObject, BaseURL: server.URL,
				Tasks: []Task{TaskMatcherExtract}, NoThinkLocation: NoThinkLocationUser,
				NoThinkFormat: NoThinkFormatDoubleNewline, AppendNoThink: true,
			}
			completion, err := client.Complete(
				context.Background(),
				system,
				"test-key",
				PromptRequest{
					Task: TaskMatcherExtract, SchemaName: "extract",
					System: llmmatch.ExtractPrompt(), User: "name",
					MaxCompletionTokens: 120, Schema: matcherExtractSchema(),
				},
			)
			if test.wantErr != "" {
				require.EqualError(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.wantText, string(completion.Text))
			require.Equal(t, test.wantModel, completion.ReturnedModel)
			require.Equal(t, test.wantProof, completion.RouteProof)
			require.Equal(t, test.wantAccounting, completion.Usage.AccountingComplete)
			require.NoError(t, completion.Usage.Validate())
			if test.checkRequestID {
				require.Equal(t, test.wantRequestID, completion.Usage.RequestID)
			}
			if test.wantRouteError != "" {
				var directReturnedModel string
				_, routeErr := auditCompletionRoute(
					system,
					ExecutionBinding{},
					completion,
					&directReturnedModel,
				)
				require.EqualError(t, routeErr, test.wantRouteError)
			}
		})
	}
}

func TestHostedClientPreservesJunkProductionRequestShape(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
		_, _ = w.Write([]byte(`{
			"id":"chat-junk","model":"gpt-5.6-terra",
			"choices":[{"message":{"content":"{\"verdict\":\"real_absent\",\"confidence\":0.9}"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5}
		}`))
	}))
	defer server.Close()

	client := &HostedClient{HTTP: server.Client()}
	system := SystemConfig{
		SystemID: "junk-production", Provider: "openai", Model: "gpt-5.6-terra",
		PromptVersion: "production", EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind: APIKindChat, OutputContract: OutputContractPromptOnly, BaseURL: server.URL,
		Tasks: []Task{TaskJunkPurge}, NoThinkLocation: NoThinkLocationSystem,
		NoThinkFormat:   NoThinkFormatSingleNewline,
		ReasoningEffort: "low", MaxCompletionTokensOverride: 512,
	}
	_, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskJunkPurge, SchemaName: "junk",
			System: junkpurge.EvaluationPrompt(), User: "name",
			MaxCompletionTokens: 120, Schema: junkPurgeSchema(),
		},
	)
	require.NoError(t, err)

	messages := requestBody["messages"].([]any)
	require.Equal(
		t,
		junkpurge.EvaluationPrompt()+"\n/no_think",
		messages[0].(map[string]any)["content"],
	)
	require.Equal(t, "name", messages[1].(map[string]any)["content"])
	require.Equal(t, false, requestBody["stream"])
	require.Equal(t, float64(512), requestBody["max_completion_tokens"])
	require.Equal(t, "low", requestBody["reasoning_effort"])
	require.NotContains(t, requestBody, "response_format")
}

func TestHostedClientResponsesUsageAndProductionPromptOnlyShape(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/responses", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
		_, _ = w.Write([]byte(`{
			"id":"resp-1","model":"gpt-test",
			"output":[{"type":"message","content":[{"type":"output_text","text":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"english-clear\"}"}]}],
			"usage":{"input_tokens":200,"output_tokens":20,"input_tokens_details":{"cached_tokens":50},"output_tokens_details":{"reasoning_tokens":3}}
		}`))
	}))
	defer server.Close()

	client := &HostedClient{HTTP: server.Client()}
	system := SystemConfig{
		SystemID: "openai", Provider: "openai", Model: "gpt-test",
		PromptVersion: "v1", EvaluationLane: EvaluationLaneProductionFidelity,
		APIKind: APIKindResponses, OutputContract: OutputContractPromptOnly, BaseURL: server.URL,
		Tasks: []Task{TaskContentFilter}, OmitMaxCompletionTokens: true,
		NoThinkLocation:    NoThinkLocationNone,
		InputUSDPerMillion: 1, CachedInputUSDPerMillion: 0.1, OutputUSDPerMillion: 5,
	}
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskContentFilter, SchemaName: "language",
			System: contentfilter.EvaluationPrompt(), User: "name",
			MaxCompletionTokens: 120, Schema: contentFilterSchema(),
		},
	)
	require.NoError(t, err)
	require.Equal(t, int64(50), completion.Usage.CachedInputTokens)
	require.Equal(t, int64(3), completion.Usage.ReasoningTokens)
	require.Equal(t, int64(255), completion.Usage.CostMicroUSD)
	require.Equal(t, CostSourceManifestEstimate, completion.Usage.CostSource)
	require.Equal(t, contentfilter.EvaluationPrompt(), requestBody["instructions"])
	require.Equal(t, "name", requestBody["input"])
	require.NotContains(t, requestBody, "text")
	require.NotContains(t, requestBody, "max_output_tokens")
}

func TestHostedClientEmbedsQueryAndCandidatesInOnePinnedRequest(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			require.Equal(t, "/generation", r.URL.Path)
			require.Equal(t, "emb-generation-1", r.URL.Query().Get("id"))
			require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"data":{
				"id":"emb-generation-1",
				"model":"test/embed",
				"provider_name":"Test Provider",
				"tokens_prompt":30,
				"tokens_completion":0,
				"native_tokens_prompt":30,
				"native_tokens_completion":0,
				"total_cost":0.00003
			}}`))
			return
		}
		require.Equal(t, "/embeddings", r.URL.Path)
		require.Empty(t, r.Header.Get("X-OpenRouter-Metadata"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
		w.Header().Set("X-Generation-Id", "emb-generation-1")
		_, _ = w.Write([]byte(`{
			"id":"emb-1","model":"test/embed","provider":"Test Provider",
			"data":[
				{"index":2,"embedding":[0,1]},
				{"index":0,"embedding":[1,0]},
				{"index":1,"embedding":[0.8,0.2]}
			],
			"usage":{"prompt_tokens":30,"total_tokens":30}
		}`))
	}))
	defer server.Close()

	client := &HostedClient{HTTP: server.Client()}
	system := SystemConfig{
		SystemID: "embed", Provider: "openrouter", Model: "test/embed",
		PromptVersion:  MatcherSpecialistAlgorithmID,
		EvaluationLane: EvaluationLaneSpecialist,
		APIKind:        APIKindEmbedding, OutputContract: OutputContractNative, BaseURL: server.URL,
		ProviderEndpoint: "test/fp8", Tasks: []Task{TaskMatcherRerank},
		InputUSDPerMillion: 1, BillingMultiplier: 1.055, ZDR: true,
		NoThinkLocation: NoThinkLocationNone,
	}
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskMatcherRerank,
			SpecializedRerank: &SpecializedRerankRequest{
				Query: "query",
				Documents: []SpecializedRerankDocument{
					{TMDBID: 101, Text: "best"},
					{TMDBID: 202, Text: "other"},
				},
			},
		},
	)
	require.NoError(t, err)
	// Confidence is derived from the integer parts-per-billion score, so it is
	// quantised to 1e-9 rather than carrying raw float64 cosine precision.
	require.JSONEq(t, `{"tmdb_id":101,"confidence":0.9701425}`, string(completion.Text))
	require.Equal(t, int64(32), completion.Usage.CostMicroUSD)
	require.Equal(t, "emb-generation-1", completion.Usage.RequestID)
	require.Equal(t, RouteProofGenerationMetadata, completion.RouteProof)
	require.Equal(t, "Test Provider", completion.ReturnedProvider)

	require.Equal(t, []any{"query", "best", "other"}, requestBody["input"])
	provider := requestBody["provider"].(map[string]any)
	require.Equal(t, []any{"test"}, provider["order"])
	require.Equal(t, []any{"test"}, provider["only"])
	require.Equal(t, []any{"fp8"}, provider["quantizations"])
	require.Equal(t, false, provider["allow_fallbacks"])
	require.Equal(t, "deny", provider["data_collection"])
	require.Equal(t, true, provider["zdr"])
}

func TestHostedClientRerankNormalizesTopDocument(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			require.Equal(t, "/generation", r.URL.Path)
			require.Equal(t, "rerank-generation-1", r.URL.Query().Get("id"))
			require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"data":{
				"id":"rerank-generation-1",
				"model":"test/rerank",
				"provider_name":"Test Provider",
				"tokens_prompt":42,
				"tokens_completion":0,
				"native_tokens_prompt":42,
				"native_tokens_completion":0,
				"total_cost":0.001
			}}`))
			return
		}
		require.Equal(t, "/rerank", r.URL.Path)
		require.Empty(t, r.Header.Get("X-OpenRouter-Metadata"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
		w.Header().Set("X-Generation-Id", "rerank-generation-1")
		_, _ = w.Write([]byte(`{
			"id":"rerank-1","model":"test/rerank","provider":"Test Provider",
			"results":[{"index":1,"relevance_score":0.91}],
			"usage":{"search_units":1,"total_tokens":42}
		}`))
	}))
	defer server.Close()

	client := &HostedClient{HTTP: server.Client()}
	system := SystemConfig{
		SystemID: "rerank", Provider: "openrouter", Model: "test/rerank",
		PromptVersion: "v1", EvaluationLane: EvaluationLaneSpecialist,
		APIKind: APIKindRerank, OutputContract: OutputContractNative, BaseURL: server.URL,
		ProviderEndpoint: "test", Tasks: []Task{TaskMatcherRerank},
		USDPerRequest: 0.001, BillingMultiplier: 1.055, ZDR: true,
		NoThinkLocation: NoThinkLocationNone,
	}
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskMatcherRerank,
			SpecializedRerank: &SpecializedRerankRequest{
				Query: "query",
				Documents: []SpecializedRerankDocument{
					{TMDBID: 101, Text: "first"},
					{TMDBID: 202, Text: "second"},
				},
			},
		},
	)
	require.NoError(t, err)
	require.JSONEq(t, `{"tmdb_id":202,"confidence":0.91}`, string(completion.Text))
	require.Equal(t, int64(1055), completion.Usage.CostMicroUSD)
	require.Equal(t, RouteProofGenerationMetadata, completion.RouteProof)
	require.Equal(t, "Test Provider", completion.ReturnedProvider)
	provider := requestBody["provider"].(map[string]any)
	require.Equal(t, []any{"test"}, provider["order"])
	require.Equal(t, []any{"test"}, provider["only"])
	require.NotContains(t, provider, "quantizations")
}

func TestHostedClientPreservesMetadataOnMalformedSuccessEnvelope(t *testing.T) {
	t.Parallel()
	specializedPrompt := PromptRequest{
		Task: TaskMatcherRerank,
		SpecializedRerank: &SpecializedRerankRequest{
			Query: "query",
			Documents: []SpecializedRerankDocument{
				{TMDBID: 101, Text: "document"},
			},
		},
	}
	tests := []struct {
		name       string
		openRouter bool
		apiKind    APIKind
		task       Task
		prompt     PromptRequest
	}{
		{
			name:       "OpenRouter chat",
			openRouter: true,
			apiKind:    APIKindChat,
			task:       TaskContentFilter,
			prompt: PromptRequest{
				Task: TaskContentFilter, SchemaName: "content",
				System: "policy", User: "name",
				MaxCompletionTokens: 120, Schema: contentFilterSchema(),
			},
		},
		{
			name:       "OpenRouter embedding",
			openRouter: true,
			apiKind:    APIKindEmbedding,
			task:       TaskMatcherRerank,
			prompt:     specializedPrompt,
		},
		{
			name:       "OpenRouter rerank",
			openRouter: true,
			apiKind:    APIKindRerank,
			task:       TaskMatcherRerank,
			prompt:     specializedPrompt,
		},
		{
			name:    "direct Responses",
			apiKind: APIKindResponses,
			task:    TaskContentFilter,
			prompt: PromptRequest{
				Task: TaskContentFilter, SchemaName: "content",
				System: "policy", User: "name",
				MaxCompletionTokens: 120, Schema: contentFilterSchema(),
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			generationID := "malformed-generation"
			requestID := "malformed-request"
			var generationCalls int
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						generationCalls++
						require.Equal(t, generationID, r.URL.Query().Get("id"))
						_, _ = w.Write([]byte(`{"data":{
							"id":"malformed-generation",
							"model":"test/model",
							"provider_name":"Test Provider",
							"native_tokens_prompt":9,
							"native_tokens_completion":3,
							"total_cost":0.000017
						}}`))
						return
					}
					w.Header().Set("X-Generation-Id", generationID)
					w.Header().Set("X-Request-Id", requestID)
					_, _ = w.Write([]byte(`{`))
				},
			))
			defer server.Close()

			system := testOpenRouterSystem(test.task)
			system.BaseURL = server.URL
			system.APIKind = test.apiKind
			if test.apiKind == APIKindEmbedding ||
				test.apiKind == APIKindRerank {
				system.EvaluationLane = EvaluationLaneSpecialist
				system.OutputContract = OutputContractNative
			}
			if test.apiKind == APIKindEmbedding {
				system.PromptVersion = MatcherSpecialistAlgorithmID
			}
			if !test.openRouter {
				system.Provider = "openai"
				system.ZDR = false
			}
			completion, err := (&HostedClient{HTTP: server.Client()}).Complete(
				context.Background(),
				system,
				"test-key",
				test.prompt,
			)
			require.EqualError(t, err, "decode_envelope")
			require.Equal(t, generationID, completion.GenerationID)
			require.Equal(t, generationID, completion.Usage.RequestID)
			if test.openRouter {
				require.Equal(t, 1, generationCalls)
				require.Equal(t, "test/model", completion.ReturnedModel)
				require.Equal(t, "Test Provider", completion.ReturnedProvider)
				require.Equal(t, RouteProofGenerationMetadata, completion.RouteProof)
				require.Equal(t, int64(18), completion.Usage.CostMicroUSD)
				require.True(t, completion.UsageCostReported)
			} else {
				require.Zero(t, generationCalls)
				require.Empty(t, completion.ReturnedModel)
				require.Equal(t, RouteProofUnverified, completion.RouteProof)
			}
		})
	}
}

func TestHostedClientReconcilesBillableOpenRouterHTTPFailure(t *testing.T) {
	var generationCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/chat/completions":
			w.Header().Set("X-Generation-Id", "failed-generation-1")
			http.Error(w, "provider unavailable", http.StatusTooManyRequests)
		case r.Method == http.MethodGet && r.URL.Path == "/generation":
			generationCalls++
			require.Equal(t, "failed-generation-1", r.URL.Query().Get("id"))
			require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
			if generationCalls == 1 {
				_, _ = w.Write([]byte(`{"data":{"id":"failed-generation-1"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{
				"id":"failed-generation-1",
				"model":"test/model",
				"provider_name":"Test Provider",
				"native_tokens_prompt":25,
				"native_tokens_completion":0,
				"native_tokens_cached":5,
				"total_cost":0.000017
			}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	client := &HostedClient{HTTP: server.Client()}
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{
			Task: TaskContentFilter, SchemaName: "content", System: "policy", User: "name",
			MaxCompletionTokens: 120, Schema: contentFilterSchema(),
		},
	)
	require.EqualError(t, err, "http_429")
	require.Equal(t, 2, generationCalls)
	require.Equal(t, "failed-generation-1", completion.Usage.RequestID)
	require.Equal(t, int64(25), completion.Usage.InputTokens)
	require.Equal(t, int64(5), completion.Usage.CachedInputTokens)
	require.Equal(t, int64(18), completion.Usage.CostMicroUSD)
	require.Equal(t, "test/model", completion.ReturnedModel)
	require.Equal(t, "Test Provider", completion.ReturnedProvider)
	require.Equal(t, RouteProofGenerationMetadata, completion.RouteProof)
}

func TestHostedClientRejectsOversizeProviderResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxProviderResponseBytes+1))
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	client := &HostedClient{HTTP: server.Client()}
	_, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{Task: TaskContentFilter},
	)
	require.EqualError(t, err, "response_too_large")
}

func TestHostedClientRejectsOversizeGenerationMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write(make([]byte, maxProviderResponseBytes+1))
			return
		}
		w.Header().Set("X-Generation-Id", "oversize-generation")
		_, _ = w.Write([]byte(`{
			"id":"chat-cache","model":"test/model",
			"choices":[{"message":{"content":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"clear\"}"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5}
		}`))
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	client := &HostedClient{HTTP: server.Client()}
	_, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		PromptRequest{Task: TaskContentFilter},
	)
	require.EqualError(t, err, "response_too_large")
}

func TestOpenRouterProviderPolicySeparatesQuantization(t *testing.T) {
	tests := []struct {
		endpoint     string
		wantProvider string
		wantQuant    string
	}{
		{endpoint: "dekallm/fp8", wantProvider: "dekallm", wantQuant: "fp8"},
		{endpoint: "deepinfra/fp32", wantProvider: "deepinfra", wantQuant: "fp32"},
		{endpoint: "perplexity/int8", wantProvider: "perplexity", wantQuant: "int8"},
		{endpoint: "azure/swedencentral", wantProvider: "azure/swedencentral"},
		{endpoint: "novita", wantProvider: "novita"},
	}
	for _, test := range tests {
		t.Run(test.endpoint, func(t *testing.T) {
			policy := openRouterProviderPolicy(
				SystemConfig{ProviderEndpoint: test.endpoint, ZDR: true},
				true,
			)
			require.Equal(t, []string{test.wantProvider}, policy["order"])
			require.Equal(t, []string{test.wantProvider}, policy["only"])
			if test.wantQuant == "" {
				require.NotContains(t, policy, "quantizations")
			} else {
				require.Equal(t, []string{test.wantQuant}, policy["quantizations"])
			}
		})
	}
}

func TestSystemAcceptsDocumentedOpenRouterEndpointVariant(t *testing.T) {
	system := testOpenRouterSystem(TaskContentFilter)
	system.ProviderEndpoint = "azure/swedencentral"
	require.NoError(t, system.Validate())
}

func TestHostedClientRefusesNonZDRSpecialistRoute(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer server.Close()

	client := &HostedClient{HTTP: server.Client()}
	_, err := client.Complete(
		context.Background(),
		SystemConfig{
			Provider: "openrouter", Model: "test/rerank",
			EvaluationLane: EvaluationLaneSpecialist,
			APIKind:        APIKindRerank, OutputContract: OutputContractNative,
			BaseURL: server.URL, ProviderEndpoint: "test", Tasks: []Task{TaskMatcherRerank},
			ZDR: false, NoThinkLocation: NoThinkLocationNone,
		},
		"test-key",
		PromptRequest{
			Task: TaskMatcherRerank,
			SpecializedRerank: &SpecializedRerankRequest{
				Query:     "query",
				Documents: []SpecializedRerankDocument{{TMDBID: 101, Text: "doc"}},
			},
		},
	)
	require.EqualError(t, err, "zdr_required")
	require.False(t, called)
}

func TestHostedClientSanitizesProviderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `secret echoed prompt and key`, http.StatusBadRequest)
	}))
	defer server.Close()

	client := &HostedClient{HTTP: &http.Client{Timeout: time.Second}}
	completion, err := client.Complete(
		context.Background(),
		SystemConfig{
			Provider: "openrouter", Model: "test",
			EvaluationLane: EvaluationLaneNormalizedStrict,
			APIKind:        APIKindChat, OutputContract: OutputContractJSONSchema,
			BaseURL: server.URL, ProviderEndpoint: "test/fp8", Tasks: []Task{TaskJunkPurge},
			StructuredOutputs: true, ZDR: true, NoThinkLocation: NoThinkLocationNone,
		},
		"secret-key",
		PromptRequest{Task: TaskJunkPurge},
	)
	require.EqualError(t, err, "http_400")
	require.NotContains(t, err.Error(), "secret")
	require.Equal(t, CostSourceUnavailable, completion.Usage.CostSource)
	require.Zero(t, completion.Usage.CostMicroUSD)
}

func TestHostedClientGenerationAuditSucceedsOnFifthMetadataOnlyAttempt(t *testing.T) {
	var postCalls atomic.Int32
	var getCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalls.Add(1)
			writeInlineOpenRouterResponseWithoutCost(t, w, "generation-fifth")
			return
		}
		call := getCalls.Add(1)
		if call < 5 {
			_, _ = w.Write([]byte(`{"data":{"id":"generation-fifth"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{
			"id":"generation-fifth","model":"test/model",
			"provider_name":"Test Provider","native_tokens_prompt":10,
			"native_tokens_completion":5,"total_cost":0.00001
		}}`))
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	client := &HostedClient{
		HTTP: server.Client(),
		generationMetadataRetryOverride: &generationMetadataRetryPolicy{
			Delays: []time.Duration{0, 0, 0, 0}, Timeout: time.Second,
		},
	}
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		contentFilterTransportPrompt(),
	)
	require.NoError(t, err)
	require.Equal(t, int32(1), postCalls.Load(), "inference must never be retried")
	require.Equal(t, int32(5), getCalls.Load())
	require.Equal(t, RouteProofGenerationMetadata, completion.RouteProof)
	require.True(t, completion.Usage.AccountingComplete)
	require.Equal(t, CostSourceProviderReported, completion.Usage.CostSource)
	require.NotNil(t, completion.RequestTiming)
	require.NoError(t, completion.RequestTiming.Validate())
	require.Equal(t, 5, completion.RequestTiming.RouteAudit.Attempts)
	require.True(t, completion.RequestTiming.RouteAudit.Succeeded)
}

func TestHostedClientKeepsVerifiedActionWhenGenerationAccountingLags(t *testing.T) {
	var postCalls atomic.Int32
	var getCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalls.Add(1)
			writeInlineOpenRouterResponseWithoutCost(t, w, "generation-lagging")
			return
		}
		getCalls.Add(1)
		_, _ = w.Write([]byte(`{"data":{"id":"generation-lagging"}}`))
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	client := &HostedClient{
		HTTP: server.Client(),
		generationMetadataRetryOverride: &generationMetadataRetryPolicy{
			Delays: []time.Duration{0, 0, 0, 0}, Timeout: time.Second,
		},
	}
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		contentFilterTransportPrompt(),
	)
	require.NoError(t, err)
	require.Equal(t, int32(1), postCalls.Load(), "inference must never be retried")
	require.Equal(t, int32(5), getCalls.Load())
	require.Equal(t, RouteProofRouterMetadata, completion.RouteProof)
	require.False(t, completion.Usage.AccountingComplete)
	require.Equal(t, CostSourceManifestEstimate, completion.Usage.CostSource)
	require.False(t, completion.UsageCostReported)
	require.Equal(t, "route_audit_unavailable", completion.RequestTiming.RouteAudit.ErrorCode)
	require.False(t, completion.RequestTiming.RouteAudit.Succeeded)
	require.Equal(t, 5, completion.RequestTiming.RouteAudit.Attempts)
}

func TestHostedClientMissingInlineRouteStillFailsClosedAfterAuditExhaustion(t *testing.T) {
	var postCalls atomic.Int32
	var getCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalls.Add(1)
			_, _ = w.Write([]byte(`{
				"id":"generation-no-route","model":"test/model",
				"choices":[{"message":{"content":"{\"is_english\":true,\"confidence\":0.9,\"reason\":\"clear\"}"}}],
				"usage":{"prompt_tokens":10,"completion_tokens":5}
			}`))
			return
		}
		getCalls.Add(1)
		_, _ = w.Write([]byte(`{"data":{"id":"generation-no-route"}}`))
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	client := &HostedClient{
		HTTP: server.Client(),
		generationMetadataRetryOverride: &generationMetadataRetryPolicy{
			Delays: []time.Duration{0, 0, 0, 0}, Timeout: time.Second,
		},
	}
	completion, err := client.Complete(
		context.Background(),
		system,
		"test-key",
		contentFilterTransportPrompt(),
	)
	require.EqualError(t, err, "route_unverifiable")
	require.Equal(t, int32(1), postCalls.Load())
	require.Equal(t, int32(5), getCalls.Load())
	require.Equal(t, "route_audit_unavailable", completion.RequestTiming.RouteAudit.ErrorCode)
}

func TestHostedClientGenerationAuditTamperIsTerminal(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{}`},
		{name: "malformed JSON", status: http.StatusOK, body: `{`},
		{name: "missing generation id", status: http.StatusOK, body: `{"data":{"model":"test/model","provider_name":"Test Provider","total_cost":0.00001}}`},
		{name: "wrong generation id", status: http.StatusOK, body: `{"data":{"id":"other","model":"test/model","provider_name":"Test Provider","total_cost":0.00001}}`},
		{name: "negative cost", status: http.StatusOK, body: `{"data":{"id":"generation-tamper","model":"test/model","provider_name":"Test Provider","total_cost":-1}}`},
		{name: "model mismatch", status: http.StatusOK, body: `{"data":{"id":"generation-tamper","model":"other/model","provider_name":"Test Provider","total_cost":0.00001}}`},
		{name: "provider mismatch", status: http.StatusOK, body: `{"data":{"id":"generation-tamper","model":"test/model","provider_name":"Other Provider","total_cost":0.00001}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var postCalls atomic.Int32
			var getCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					postCalls.Add(1)
					writeInlineOpenRouterResponseWithoutCost(t, w, "generation-tamper")
					return
				}
				getCalls.Add(1)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			system := testOpenRouterSystem(TaskContentFilter)
			system.BaseURL = server.URL
			client := &HostedClient{
				HTTP: server.Client(),
				generationMetadataRetryOverride: &generationMetadataRetryPolicy{
					Delays: []time.Duration{0, 0, 0, 0}, Timeout: time.Second,
				},
			}
			completion, err := client.Complete(
				context.Background(),
				system,
				"test-key",
				contentFilterTransportPrompt(),
			)
			require.EqualError(t, err, "route_unverifiable")
			require.Equal(t, int32(1), postCalls.Load())
			require.Equal(t, int32(1), getCalls.Load(), "terminal integrity failures must not retry")
			require.False(t, completion.RequestTiming.RouteAudit.Succeeded)
			require.Equal(t, 1, completion.RequestTiming.RouteAudit.Attempts)
		})
	}
}

func TestHostedClientGenerationAuditHonorsCancellationAndDeadlines(t *testing.T) {
	t.Run("parent cancellation during backoff", func(t *testing.T) {
		firstAudit := make(chan struct{})
		var once atomic.Bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				writeInlineOpenRouterResponseWithoutCost(t, w, "generation-cancel")
				return
			}
			if once.CompareAndSwap(false, true) {
				close(firstAudit)
			}
			_, _ = w.Write([]byte(`{"data":{"id":"generation-cancel"}}`))
		}))
		defer server.Close()

		system := testOpenRouterSystem(TaskContentFilter)
		system.BaseURL = server.URL
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-firstAudit
			cancel()
		}()
		completion, err := (&HostedClient{
			HTTP: server.Client(),
			generationMetadataRetryOverride: &generationMetadataRetryPolicy{
				Delays: []time.Duration{time.Hour}, Timeout: time.Second,
			},
		}).Complete(ctx, system, "test-key", contentFilterTransportPrompt())
		require.EqualError(t, err, "canceled")
		require.Equal(t, "canceled", completion.RequestTiming.RouteAudit.ErrorCode)
		require.Equal(t, 1, completion.RequestTiming.RouteAudit.Attempts)
	})

	t.Run("separate audit deadline during backoff", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				writeInlineOpenRouterResponseWithoutCost(t, w, "generation-audit-timeout")
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"generation-audit-timeout"}}`))
		}))
		defer server.Close()

		system := testOpenRouterSystem(TaskContentFilter)
		system.BaseURL = server.URL
		completion, err := (&HostedClient{
			HTTP: server.Client(),
			generationMetadataRetryOverride: &generationMetadataRetryPolicy{
				Delays: []time.Duration{time.Hour}, Timeout: 20 * time.Millisecond,
			},
		}).Complete(context.Background(), system, "test-key", contentFilterTransportPrompt())
		require.NoError(t, err, "verified inline action survives accounting-audit timeout")
		require.Equal(t, "route_audit_timeout", completion.RequestTiming.RouteAudit.ErrorCode)
		require.False(t, completion.RequestTiming.RouteAudit.Succeeded)
		require.Less(t, completion.RequestTiming.ElapsedMS, completion.RequestTiming.TotalElapsedMS)
		require.GreaterOrEqual(t, completion.RequestTiming.RouteAudit.ElapsedMS, int64(20))
	})

	t.Run("separate audit deadline while reading metadata", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				writeInlineOpenRouterResponseWithoutCost(t, w, "generation-audit-read-timeout")
				return
			}
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		}))
		defer server.Close()

		system := testOpenRouterSystem(TaskContentFilter)
		system.BaseURL = server.URL
		completion, err := (&HostedClient{
			HTTP: server.Client(),
			generationMetadataRetryOverride: &generationMetadataRetryPolicy{
				Timeout: 20 * time.Millisecond,
			},
		}).Complete(context.Background(), system, "test-key", contentFilterTransportPrompt())
		require.NoError(t, err)
		require.Equal(t, "route_audit_timeout", completion.RequestTiming.RouteAudit.ErrorCode)
		require.Equal(t, 1, completion.RequestTiming.RouteAudit.Attempts)
	})

	t.Run("parent deadline remains authoritative", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				writeInlineOpenRouterResponseWithoutCost(t, w, "generation-parent-timeout")
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"generation-parent-timeout"}}`))
		}))
		defer server.Close()

		system := testOpenRouterSystem(TaskContentFilter)
		system.BaseURL = server.URL
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		completion, err := (&HostedClient{
			HTTP: server.Client(),
			generationMetadataRetryOverride: &generationMetadataRetryPolicy{
				Delays: []time.Duration{time.Hour}, Timeout: time.Second,
			},
		}).Complete(ctx, system, "test-key", contentFilterTransportPrompt())
		require.EqualError(t, err, "timeout")
		require.Equal(t, "timeout", completion.RequestTiming.RouteAudit.ErrorCode)
	})
}

func TestHostedClientClassifiesPrimaryBodyReadDeadlineAsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	system := testOpenRouterSystem(TaskContentFilter)
	system.BaseURL = server.URL
	system.RequestTimeoutMS = 20
	completion, err := (&HostedClient{HTTP: server.Client()}).Complete(
		context.Background(),
		system,
		"test-key",
		contentFilterTransportPrompt(),
	)
	require.EqualError(t, err, "timeout")
	require.NotNil(t, completion.RequestTiming)
	require.Equal(t, int64(20), completion.RequestTiming.DeadlineMS)
	require.Nil(t, completion.RequestTiming.RouteAudit)
}

func writeInlineOpenRouterResponseWithoutCost(
	t *testing.T,
	w http.ResponseWriter,
	generationID string,
) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"id":    generationID,
		"model": "test/model",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"content": `{"is_english":true,"confidence":0.9,"reason":"clear"}`,
			},
		}},
		"usage": map[string]any{
			"prompt_tokens": 10, "completion_tokens": 5,
		},
		"openrouter_metadata": map[string]any{
			"endpoints": map[string]any{
				"available": []any{map[string]any{
					"provider": "Test Provider", "model": "test/model", "selected": true,
				}},
			},
		},
	}))
}

func contentFilterTransportPrompt() PromptRequest {
	return PromptRequest{
		Task: TaskContentFilter, SchemaName: "content", System: "policy", User: "name",
		MaxCompletionTokens: 120, Schema: contentFilterSchema(),
	}
}
