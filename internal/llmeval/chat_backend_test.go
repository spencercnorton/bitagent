package llmeval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/stretchr/testify/require"
)

func TestOllamaControlsCannotClaimHistoricalDeployedWireFidelity(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	for _, lane := range []EvaluationLane{EvaluationLaneProductionFidelity, EvaluationLaneShadowFidelity} {
		for _, task := range []Task{TaskMatcherExtract, TaskMatcherRerank} {
			for _, controls := range []string{"max_tokens", "reasoning_effort", "both"} {
				system := testShadowFidelitySystem(task)
				system.EvaluationLane, system.BaseURL = lane, srv.URL
				if lane == EvaluationLaneProductionFidelity {
					system.Provider, system.ProviderEndpoint = "openai", ""
				}
				if controls != "reasoning_effort" {
					system.UseMaxTokens = true
				}
				if controls != "max_tokens" {
					system.ReasoningEffort = "none"
				}
				client := &HostedClient{HTTP: srv.Client()}
				_, err := client.Complete(context.Background(), system, "synthetic-key", PromptRequest{Task: task, System: "policy", User: "synthetic", MaxCompletionTokens: 120})
				require.ErrorContains(t, err, "production matcher request controls do not match the deployed contract")
			}
		}
	}
	require.Zero(t, calls.Load(), "incompatible fidelity controls fail before any HTTP request")
}

func TestHistoricalMatcherManifestRejectsUnknownBackend(t *testing.T) {
	const raw = `{"schema_version":1,"systems":[{"system_id":"synthetic","chat_backend":"ollama"}]}`
	_, err := decodeSystemManifest([]byte(raw))
	require.ErrorContains(t, err, `unknown field "chat_backend"`)
}

func TestHistoricalMatcherFidelityFullBytesRemainUnchanged(t *testing.T) {
	system := testShadowFidelitySystem(TaskMatcherExtract)
	system.EvaluationLane, system.Provider, system.ProviderEndpoint, system.Model = EvaluationLaneProductionFidelity, "openai", "", "synthetic-model"
	_, body, err := GenerativeRequestBody(system, PromptRequest{Task: TaskMatcherExtract, System: llmmatch.ExtractPrompt(), User: "synthetic", MaxCompletionTokens: 120})
	require.NoError(t, err)
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	prompt, err := json.Marshal(llmmatch.ExtractPrompt())
	require.NoError(t, err)
	expected := fmt.Sprintf(`{"model":"synthetic-model","messages":[{"role":"system","content":%s},{"role":"user","content":"synthetic\n\n/no_think"}],"response_format":{"type":"json_object"},"max_completion_tokens":120}`, prompt)
	require.Equal(t, expected, string(raw))
}
