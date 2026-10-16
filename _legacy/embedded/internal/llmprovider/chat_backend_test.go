package llmprovider

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOllamaBackendRequiresExplicitCompatibleRoute(t *testing.T) {
	for _, endpoint := range []string{
		"http://127.0.0.1:11434/v1/chat/completions", "http://localhost:11434/v1/chat/completions",
		"http://[::1]:11434/v1/chat/completions", "https://models.example.org/v1/chat/completions",
	} {
		require.NoError(t, ValidateChatBackend(ChatBackendOllama, endpoint, "", false))
	}
	for _, endpoint := range []string{
		"https://api.openai.com/v1/chat/completions", "https://API.OPENAI.COM./v1/chat/completions",
		"https://openrouter.ai/v1/chat/completions", "https://OPENROUTER.AI./v1/chat/completions",
		"https://user:secret@models.example.org/v1/chat/completions",
		"https://models.example.org/v1/chat/completions?route=x", "https://models.example.org/v1/chat/completions?",
		"https://models.example.org/v1/chat/completions#x", "https://models.example.org/v1/chat/completions#",
		"https://models.example.org/v1/chat%2Fcompletions", "https://models.example.org/api/chat",
		"http://models.example.org/v1/chat/completions", "file:///v1/chat/completions", "",
	} {
		t.Run(endpoint, func(t *testing.T) { require.Error(t, ValidateChatBackend(ChatBackendOllama, endpoint, "", false)) })
	}
	endpoint := "http://127.0.0.1:11434/v1/chat/completions"
	require.Error(t, ValidateChatBackend(ChatBackendOllama, endpoint, "provider", false))
	require.Error(t, ValidateChatBackend(ChatBackendOllama, endpoint, "", true))
	for _, backend := range []ChatBackend{"guess", "OLLAMA", " ollama", "ollama "} {
		require.Error(t, ValidateChatBackend(backend, endpoint, "", false))
	}
}

func TestOpenAIBackendAddsNoNewRouteValidation(t *testing.T) {
	// Existing stage validators remain authoritative for their old dialect.
	for _, backend := range []ChatBackend{"", ChatBackendOpenAI} {
		require.Equal(t, ChatBackendOpenAI, backend.Effective())
		require.NoError(t, ValidateChatBackend(backend, "legacy caller route", "provider", true))
	}
}
