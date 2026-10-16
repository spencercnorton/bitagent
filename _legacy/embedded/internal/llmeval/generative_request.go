package llmeval

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
)

// GenerativeRequestBody returns the exact provider request body used by the
// synchronous runner and by eligible normalized OpenAI Batch controls. Keeping
// one builder prevents either transport from drifting from its manifest-bound
// Chat or Responses contract. Batch admission independently rejects every
// production-fidelity system because it cannot reproduce a live deadline.
func GenerativeRequestBody(
	system SystemConfig,
	prompt PromptRequest,
) (string, any, error) {
	if !system.SupportsTask(prompt.Task) {
		return "", nil, &CallError{Code: "unsupported_task"}
	}
	if system.UsesDeployedWireContract() {
		endpoint, body, err := productionFidelityRequestBody(system, prompt)
		if err != nil || system.EvaluationLane != EvaluationLaneShadowFidelity {
			return endpoint, body, err
		}
		body, err = injectShadowFidelityProviderPolicy(system, body)
		if err != nil {
			return "", nil, err
		}
		return endpoint, body, nil
	}
	switch system.APIKind {
	case APIKindChat:
		return "/v1/chat/completions", chatRequestBody(system, prompt), nil
	case APIKindResponses:
		return "/v1/responses", responsesRequestBody(system, prompt), nil
	default:
		return "", nil, fmt.Errorf(
			"api kind %q is not a generative Chat or Responses request",
			system.APIKind,
		)
	}
}

func productionFidelityRequestBody(
	system SystemConfig,
	prompt PromptRequest,
) (string, any, error) {
	if err := validateDeployedWireRequestControls(system, prompt.Task); err != nil {
		return "", nil, err
	}
	var (
		endpoint string
		raw      []byte
		err      error
	)
	switch prompt.Task {
	case TaskMatcherExtract, TaskMatcherRerank:
		expectedPrompt := llmmatch.ExtractPrompt()
		if prompt.Task == TaskMatcherRerank {
			expectedPrompt = llmmatch.RerankPrompt()
		}
		if prompt.System != expectedPrompt {
			return "", nil, fmt.Errorf(
				"production matcher system prompt does not match the deployed contract",
			)
		}
		endpoint = "/v1/chat/completions"
		raw, err = llmmatch.EvaluationChatRequestJSON(
			system.Model,
			prompt.System,
			prompt.User,
			prompt.MaxCompletionTokens,
		)

	case TaskContentFilter:
		if prompt.System != contentfilter.EvaluationPrompt() {
			return "", nil, fmt.Errorf(
				"production content-filter system prompt does not match the deployed contract",
			)
		}
		if system.APIKind == APIKindChat {
			endpoint = "/v1/chat/completions"
			raw, err = contentfilter.EvaluationChatRequestJSON(
				system.Model,
				prompt.User,
			)
		} else {
			endpoint = "/v1/responses"
			raw, err = contentfilter.EvaluationResponsesRequestJSON(
				system.Model,
				prompt.User,
			)
		}

	case TaskJunkPurge:
		if prompt.System != junkpurge.EvaluationPrompt() {
			return "", nil, fmt.Errorf(
				"production junk-purge system prompt does not match the deployed contract",
			)
		}
		endpoint = "/v1/chat/completions"
		raw, err = junkpurge.EvaluationChatRequestJSON(system.Model, prompt.User)

	default:
		return "", nil, fmt.Errorf(
			"task %q has no production-fidelity generative contract",
			prompt.Task,
		)
	}
	if err != nil {
		return "", nil, fmt.Errorf("serialize production request: %w", err)
	}
	return endpoint, json.RawMessage(raw), nil
}

func validateDeployedWireRequestControls(system SystemConfig, task Task) error {
	switch task {
	case TaskMatcherExtract, TaskMatcherRerank:
		if system.APIKind != APIKindChat ||
			system.OutputContract != OutputContractJSONObject ||
			system.NoThinkLocation != NoThinkLocationUser ||
			system.NoThinkFormat != NoThinkFormatDoubleNewline ||
			!system.AppendNoThink ||
			system.UseMaxTokens ||
			system.OmitMaxCompletionTokens ||
			system.MaxCompletionTokensOverride != 0 ||
			system.ReasoningEffort != "" ||
			system.Temperature != nil ||
			system.Seed != nil {
			return fmt.Errorf(
				"production matcher request controls do not match the deployed contract",
			)
		}
	case TaskContentFilter:
		commonControlsMatch := system.OutputContract == OutputContractPromptOnly &&
			!system.AppendNoThink &&
			!system.UseMaxTokens &&
			system.OmitMaxCompletionTokens &&
			system.MaxCompletionTokensOverride == 0 &&
			system.ReasoningEffort == "" &&
			system.Temperature == nil &&
			system.Seed == nil
		responsesControlsMatch := system.APIKind == APIKindResponses &&
			system.NoThinkLocation == NoThinkLocationNone &&
			system.NoThinkFormat == NoThinkFormatNone
		chatControlsMatch := system.APIKind == APIKindChat &&
			system.NoThinkLocation == NoThinkLocationSystem &&
			system.NoThinkFormat == NoThinkFormatSingleNewline
		if !commonControlsMatch ||
			(!responsesControlsMatch && !chatControlsMatch) {
			return fmt.Errorf(
				"production content-filter request controls do not match the deployed contract",
			)
		}
	case TaskJunkPurge:
		if system.APIKind != APIKindChat ||
			system.OutputContract != OutputContractPromptOnly ||
			system.NoThinkLocation != NoThinkLocationSystem ||
			system.NoThinkFormat != NoThinkFormatSingleNewline ||
			system.AppendNoThink ||
			system.UseMaxTokens ||
			system.OmitMaxCompletionTokens ||
			system.MaxCompletionTokensOverride != 512 ||
			system.ReasoningEffort != "low" ||
			system.Temperature != nil ||
			system.Seed != nil {
			return fmt.Errorf(
				"production junk-purge request controls do not match the deployed contract",
			)
		}
	default:
		return fmt.Errorf(
			"task %q has no production-fidelity generative contract",
			task,
		)
	}
	return nil
}

// injectShadowFidelityProviderPolicy preserves every byte produced by the
// deployed request builder and appends the sole evaluator-only JSON member.
// This makes a byte projection (remove provider) identical to production while
// still binding the shadow request to one ZDR provider and, when the route
// snapshot exposes one, its explicit quantization.
func injectShadowFidelityProviderPolicy(
	system SystemConfig,
	body any,
) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("serialize shadow request: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	var object map[string]json.RawMessage
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' ||
		json.Unmarshal(trimmed, &object) != nil {
		return nil, fmt.Errorf("serialize shadow request: deployed body is not a JSON object")
	}
	if _, exists := object["provider"]; exists {
		return nil, fmt.Errorf("serialize shadow request: deployed body already contains provider policy")
	}
	policy, err := json.Marshal(openRouterProviderPolicy(system, true))
	if err != nil {
		return nil, fmt.Errorf("serialize shadow request provider policy: %w", err)
	}

	result := make([]byte, 0, len(trimmed)+len(policy)+12)
	result = append(result, trimmed[:len(trimmed)-1]...)
	if len(object) != 0 {
		result = append(result, ',')
	}
	result = append(result, `"provider":`...)
	result = append(result, policy...)
	result = append(result, '}')
	return json.RawMessage(result), nil
}

func chatRequestBody(system SystemConfig, prompt PromptRequest) map[string]any {
	systemText := prompt.System
	user := prompt.User
	noThinkSuffix := system.noThinkSuffix()
	switch system.NoThinkLocation {
	case NoThinkLocationSystem:
		systemText += noThinkSuffix
	case NoThinkLocationUser:
		user += noThinkSuffix
	}
	body := map[string]any{
		"model": system.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemText},
			{"role": "user", "content": user},
		},
		"stream": false,
	}
	if !system.OmitMaxCompletionTokens {
		tokenParameter := "max_completion_tokens"
		if system.UseMaxTokens {
			tokenParameter = "max_tokens"
		}
		body[tokenParameter] = completionTokenLimit(system, prompt)
	}
	switch system.OutputContract {
	case OutputContractJSONSchema:
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   prompt.SchemaName,
				"strict": true,
				"schema": prompt.Schema,
			},
		}
	case OutputContractJSONObject:
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if system.ReasoningEffort != "" {
		if system.Provider == "openrouter" {
			body["reasoning"] = map[string]string{"effort": system.ReasoningEffort}
		} else {
			body["reasoning_effort"] = system.ReasoningEffort
		}
	}
	if system.Temperature != nil {
		body["temperature"] = *system.Temperature
	}
	if system.Seed != nil {
		body["seed"] = *system.Seed
	}
	if system.Provider == "openrouter" {
		body["provider"] = openRouterProviderPolicy(system, true)
	}
	return body
}

func responsesRequestBody(
	system SystemConfig,
	prompt PromptRequest,
) map[string]any {
	instructions := prompt.System
	input := prompt.User
	noThinkSuffix := system.noThinkSuffix()
	switch system.NoThinkLocation {
	case NoThinkLocationSystem:
		instructions += noThinkSuffix
	case NoThinkLocationUser:
		input += noThinkSuffix
	}
	body := map[string]any{
		"model":        system.Model,
		"instructions": instructions,
		"input":        input,
	}
	if !system.OmitMaxCompletionTokens {
		body["max_output_tokens"] = completionTokenLimit(system, prompt)
	}
	switch system.OutputContract {
	case OutputContractJSONSchema:
		body["text"] = map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   prompt.SchemaName,
				"strict": true,
				"schema": prompt.Schema,
			},
		}
	case OutputContractJSONObject:
		body["text"] = map[string]any{
			"format": map[string]string{"type": "json_object"},
		}
	}
	if system.ReasoningEffort != "" {
		body["reasoning"] = map[string]string{"effort": system.ReasoningEffort}
	}
	return body
}
