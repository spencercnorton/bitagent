package llmprovider

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateDataSharingRouteFailsClosed(t *testing.T) {
	require.NoError(t, ValidateDataSharingChatEndpoint(
		true, OpenAIChatEndpoint, OpenAIDataSharingModel, "key", "",
	))
	require.NoError(t, ValidateDataSharingChatBaseURL(
		true, OpenAIBaseURL, "chat", OpenAIDataSharingModel, "key", "",
	))

	for name, validate := range map[string]func() error{
		"wrong endpoint": func() error {
			return ValidateDataSharingChatEndpoint(true, "https://example.invalid/v1/chat/completions", OpenAIDataSharingModel, "key", "")
		},
		"wrong base": func() error {
			return ValidateDataSharingChatBaseURL(true, OpenAIBaseURL+"/", "chat", OpenAIDataSharingModel, "key", "")
		},
		"wrong style": func() error {
			return ValidateDataSharingChatBaseURL(true, OpenAIBaseURL, "responses", OpenAIDataSharingModel, "key", "")
		},
		"wrong model": func() error {
			return ValidateDataSharingChatEndpoint(true, OpenAIChatEndpoint, "gpt-6-astra", "key", "")
		},
		"missing key": func() error {
			return ValidateDataSharingChatEndpoint(true, OpenAIChatEndpoint, OpenAIDataSharingModel, " ", "")
		},
		"router provider": func() error {
			return ValidateDataSharingChatEndpoint(true, OpenAIChatEndpoint, OpenAIDataSharingModel, "key", "azure/us")
		},
	} {
		t.Run(name, func(t *testing.T) { require.Error(t, validate()) })
	}
}

func TestApplyDataSharingChatOptions(t *testing.T) {
	body := map[string]any{"model": OpenAIDataSharingModel}
	ApplyDataSharingChatOptions(body)
	require.Equal(t, OpenAIReasoningEffort, body["reasoning_effort"])
	require.Equal(t, false, body["store"])
}
