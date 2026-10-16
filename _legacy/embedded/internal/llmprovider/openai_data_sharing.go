// Package llmprovider contains provider contracts shared by BitAgent's LLM
// stages. It deliberately does not perform provider calls.
package llmprovider

import (
	"fmt"
	"strings"
)

const (
	// OpenAIDataSharingModel is the strongest model currently admitted by the
	// OpenAI API data-sharing incentive. The incentive is configured on the
	// OpenAI project; this constant only makes BitAgent's intended route
	// explicit and auditable.
	OpenAIDataSharingModel = "gpt-5.6-sol"
	OpenAIChatEndpoint     = "https://api.openai.com/v1/chat/completions"
	OpenAIBaseURL          = "https://api.openai.com/v1"
	OpenAIReasoningEffort  = "none"
)

// ValidateDataSharingChatEndpoint fails closed when a stage claiming the
// data-sharing route could actually egress through another endpoint/model or
// through OpenRouter. A valid key still has to belong to an opted-in OpenAI
// project; deployment verification proves that separately from usage receipts.
func ValidateDataSharingChatEndpoint(
	enabled bool,
	endpoint, model, apiKey, openrouterProvider string,
) error {
	if !enabled {
		return nil
	}
	if endpoint != OpenAIChatEndpoint {
		return fmt.Errorf("OpenAI data-sharing route requires exact chat endpoint %s", OpenAIChatEndpoint)
	}
	return validateDataSharingIdentity(model, apiKey, openrouterProvider)
}

// ValidateDataSharingChatBaseURL is the API-root sibling used by clients that
// append /chat/completions themselves.
func ValidateDataSharingChatBaseURL(
	enabled bool,
	baseURL, apiStyle, model, apiKey, openrouterProvider string,
) error {
	if !enabled {
		return nil
	}
	if baseURL != OpenAIBaseURL {
		return fmt.Errorf("OpenAI data-sharing route requires exact API base URL %s", OpenAIBaseURL)
	}
	if apiStyle != "chat" {
		return fmt.Errorf("OpenAI data-sharing route requires api_style=chat")
	}
	return validateDataSharingIdentity(model, apiKey, openrouterProvider)
}

func validateDataSharingIdentity(model, apiKey, openrouterProvider string) error {
	if model != OpenAIDataSharingModel {
		return fmt.Errorf("OpenAI data-sharing route requires model %s", OpenAIDataSharingModel)
	}
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("OpenAI data-sharing route requires an API key")
	}
	if openrouterProvider != "" {
		return fmt.Errorf("OpenAI data-sharing route cannot use an OpenRouter provider")
	}
	return nil
}

// ApplyDataSharingChatOptions adds the exact low-overhead direct OpenAI request
// controls. store=false avoids retaining a retrievable response object; data
// sharing remains governed independently by the OpenAI project setting.
func ApplyDataSharingChatOptions(body map[string]any) {
	body["reasoning_effort"] = OpenAIReasoningEffort
	body["store"] = false
}
