package contentfilter

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvaluationCaptureUsesExactResolvedRequestShape(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.LLMEnabled = true
	cfg.LLMBaseURL = ""
	filter := NewWithLLM(cfg, &fakeLLM{}, LLMCallbacks{})
	input := Input{Title: "A Public Title 2026"}
	require.True(t, filter.CaptureLLMEligible(input))

	contract, err := filter.EvaluationCapture(input)
	require.NoError(t, err)
	assert.Equal(t, apiStyleResponses, contract.APIStyle)
	assert.Equal(t, llmInstructions, contract.SystemPrompt)
	expected, err := json.Marshal(
		responsesRequestBodyBounded(cfg.LLMModel, input.Title, cfg.LLMMaxOutputTokens),
	)
	require.NoError(t, err)
	assert.JSONEq(t, string(expected), string(contract.ModelInputJSON))
	assert.JSONEq(
		t,
		`{
			"title":"A Public Title 2026",
			"min_confidence":0.85,
			"live":false,
			"eligibility_input":{
				"private":false,
				"primary_extension":"",
				"all_extensions":[],
				"content_type":"",
				"languages":[]
			}
		}`,
		string(contract.TaskInputJSON),
	)
}

func TestCaptureLLMEligibleRequiresDeployedClient(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.LLMEnabled = true
	filter := New(cfg)
	assert.False(t, filter.CaptureLLMEligible(Input{Title: "Public Title"}))
	// Diagnostic export eligibility deliberately remains independent of live
	// client wiring.
	assert.True(t, filter.EvaluationLLMEligible(Input{Title: "Public Title"}))
}
