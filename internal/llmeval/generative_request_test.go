package llmeval

import (
	"encoding/json"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
	"github.com/stretchr/testify/require"
)

func TestProductionFidelityRequestsAreProductionBuilderBytes(t *testing.T) {
	t.Run("matcher chat omits stream and keeps blank-line no-think", func(t *testing.T) {
		system := SystemConfig{
			Model:           "gpt-5.4-nano",
			EvaluationLane:  EvaluationLaneProductionFidelity,
			APIKind:         APIKindChat,
			OutputContract:  OutputContractJSONObject,
			Tasks:           []Task{TaskMatcherExtract},
			NoThinkLocation: NoThinkLocationUser,
			NoThinkFormat:   NoThinkFormatDoubleNewline,
			AppendNoThink:   true,
		}
		prompt := PromptRequest{
			Task:                TaskMatcherExtract,
			System:              llmmatch.ExtractPrompt(),
			User:                "release_name: Example.2024",
			MaxCompletionTokens: 120,
		}

		endpoint, body, err := GenerativeRequestBody(system, prompt)
		require.NoError(t, err)
		require.Equal(t, "/v1/chat/completions", endpoint)
		actual, err := json.Marshal(body)
		require.NoError(t, err)
		expected, err := llmmatch.EvaluationChatRequestJSON(
			system.Model,
			prompt.System,
			prompt.User,
			prompt.MaxCompletionTokens,
		)
		require.NoError(t, err)
		require.Equal(t, expected, actual)

		var decoded map[string]any
		require.NoError(t, json.Unmarshal(actual, &decoded))
		require.NotContains(t, decoded, "stream")
		messages := decoded["messages"].([]any)
		require.Equal(
			t,
			prompt.User+"\n\n/no_think",
			messages[1].(map[string]any)["content"],
		)
	})

	t.Run("junk chat includes stream and single-newline no-think", func(t *testing.T) {
		system := SystemConfig{
			Model:                       "gpt-5.6-terra",
			EvaluationLane:              EvaluationLaneProductionFidelity,
			APIKind:                     APIKindChat,
			OutputContract:              OutputContractPromptOnly,
			Tasks:                       []Task{TaskJunkPurge},
			NoThinkLocation:             NoThinkLocationSystem,
			NoThinkFormat:               NoThinkFormatSingleNewline,
			MaxCompletionTokensOverride: 512,
			ReasoningEffort:             "low",
		}
		prompt := PromptRequest{
			Task:                TaskJunkPurge,
			System:              junkpurge.EvaluationPrompt(),
			User:                "Example.2024",
			MaxCompletionTokens: 120,
		}

		endpoint, body, err := GenerativeRequestBody(system, prompt)
		require.NoError(t, err)
		require.Equal(t, "/v1/chat/completions", endpoint)
		actual, err := json.Marshal(body)
		require.NoError(t, err)
		expected, err := junkpurge.EvaluationChatRequestJSON(
			system.Model,
			prompt.User,
		)
		require.NoError(t, err)
		require.Equal(t, expected, actual)

		var decoded map[string]any
		require.NoError(t, json.Unmarshal(actual, &decoded))
		require.Equal(t, false, decoded["stream"])
		messages := decoded["messages"].([]any)
		require.Equal(
			t,
			prompt.System+"\n/no_think",
			messages[0].(map[string]any)["content"],
		)
	})

	t.Run("content responses omits output cap and text contract", func(t *testing.T) {
		system := SystemConfig{
			Model:                   "gpt-5.6-luna",
			EvaluationLane:          EvaluationLaneProductionFidelity,
			APIKind:                 APIKindResponses,
			OutputContract:          OutputContractPromptOnly,
			Tasks:                   []Task{TaskContentFilter},
			NoThinkLocation:         NoThinkLocationNone,
			OmitMaxCompletionTokens: true,
		}
		prompt := PromptRequest{
			Task:                TaskContentFilter,
			System:              contentfilter.EvaluationPrompt(),
			User:                "Example.2024",
			MaxCompletionTokens: 120,
		}

		endpoint, body, err := GenerativeRequestBody(system, prompt)
		require.NoError(t, err)
		require.Equal(t, "/v1/responses", endpoint)
		actual, err := json.Marshal(body)
		require.NoError(t, err)
		expected, err := contentfilter.EvaluationResponsesRequestJSON(
			system.Model,
			prompt.User,
		)
		require.NoError(t, err)
		require.Equal(t, expected, actual)
	})

	t.Run("content chat is the production builder byte for byte", func(t *testing.T) {
		system := SystemConfig{
			Model:                   "amazon/nova-micro-v1",
			EvaluationLane:          EvaluationLaneProductionFidelity,
			APIKind:                 APIKindChat,
			OutputContract:          OutputContractPromptOnly,
			Tasks:                   []Task{TaskContentFilter},
			NoThinkLocation:         NoThinkLocationSystem,
			NoThinkFormat:           NoThinkFormatSingleNewline,
			OmitMaxCompletionTokens: true,
		}
		prompt := PromptRequest{
			Task:                TaskContentFilter,
			System:              contentfilter.EvaluationPrompt(),
			User:                "Example.2024",
			MaxCompletionTokens: 120,
		}

		endpoint, body, err := GenerativeRequestBody(system, prompt)
		require.NoError(t, err)
		require.Equal(t, "/v1/chat/completions", endpoint)
		actual, err := json.Marshal(body)
		require.NoError(t, err)
		expected, err := contentfilter.EvaluationChatRequestJSON(
			system.Model,
			prompt.User,
		)
		require.NoError(t, err)
		require.Equal(t, expected, actual)
	})
}

func TestShadowFidelityRequestsAreProductionBytesPlusProviderPolicy(t *testing.T) {
	tests := []struct {
		name   string
		task   Task
		prompt PromptRequest
	}{
		{
			name: "matcher extract",
			task: TaskMatcherExtract,
			prompt: PromptRequest{
				Task: TaskMatcherExtract, System: llmmatch.ExtractPrompt(),
				User: "release_name: Example.2024", MaxCompletionTokens: 120,
			},
		},
		{
			name: "matcher rerank",
			task: TaskMatcherRerank,
			prompt: PromptRequest{
				Task: TaskMatcherRerank, System: llmmatch.RerankPrompt(),
				User: "release_name: Example.2024", MaxCompletionTokens: 60,
			},
		},
		{
			name: "content filter",
			task: TaskContentFilter,
			prompt: PromptRequest{
				Task: TaskContentFilter, System: contentfilter.EvaluationPrompt(),
				User: "Example.2024", MaxCompletionTokens: 120,
			},
		},
		{
			name: "junk purge",
			task: TaskJunkPurge,
			prompt: PromptRequest{
				Task: TaskJunkPurge, System: junkpurge.EvaluationPrompt(),
				User: "Example.2024", MaxCompletionTokens: 120,
			},
		},
	}
	const policySuffix = `,"provider":{"allow_fallbacks":false,"data_collection":"deny",` +
		`"only":["deepinfra"],"order":["deepinfra"],` +
		`"quantizations":["fp8"],"require_parameters":true,"zdr":true}}`
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			shadow := testShadowFidelitySystem(test.task)
			endpoint, body, err := GenerativeRequestBody(shadow, test.prompt)
			require.NoError(t, err)
			require.Equal(t, "/v1/chat/completions", endpoint)
			actual, err := json.Marshal(body)
			require.NoError(t, err)

			production := shadow
			production.EvaluationLane = EvaluationLaneProductionFidelity
			production.Provider = "openai"
			production.ProviderEndpoint = ""
			production.ZDR = false
			_, productionBody, err := GenerativeRequestBody(
				production,
				test.prompt,
			)
			require.NoError(t, err)
			expected, err := json.Marshal(productionBody)
			require.NoError(t, err)
			require.NotEmpty(t, expected)
			require.Equal(
				t,
				string(expected[:len(expected)-1])+policySuffix,
				string(actual),
			)

			var shadowObject map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(actual, &shadowObject))
			provider := shadowObject["provider"]
			require.JSONEq(t, `{
				"order":["deepinfra"],
				"only":["deepinfra"],
				"allow_fallbacks":false,
				"require_parameters":true,
				"data_collection":"deny",
				"zdr":true,
				"quantizations":["fp8"]
			}`, string(provider))
			delete(shadowObject, "provider")
			projected, err := json.Marshal(shadowObject)
			require.NoError(t, err)
			require.JSONEq(t, string(expected), string(projected))
		})
	}
}

func TestShadowFidelityProviderPolicyCannotOverwriteDeployedBody(t *testing.T) {
	_, err := injectShadowFidelityProviderPolicy(
		testShadowFidelitySystem(TaskMatcherExtract),
		json.RawMessage(`{"model":"test","provider":{"only":["other"]}}`),
	)
	require.ErrorContains(t, err, "already contains provider policy")
}

func TestShadowFidelityProviderOnlyEndpointIsRejected(t *testing.T) {
	system := testShadowFidelitySystem(TaskContentFilter)
	system.ProviderEndpoint = "amazon-bedrock"
	require.ErrorContains(
		t,
		system.Validate(),
		"explicit recognized quantization",
	)
}
