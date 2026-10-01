package llmprovider

import (
	"fmt"
	"net/url"
	"strings"
)

// ChatBackend selects a request dialect without choosing a provider or route.
// Empty preserves the existing OpenAI-compatible request contract.
type ChatBackend string

const (
	ChatBackendOpenAI ChatBackend = "openai"
	ChatBackendOllama ChatBackend = "ollama"
)

// Effective returns the legacy dialect for an omitted backend.
func (b ChatBackend) Effective() ChatBackend {
	if b == "" {
		return ChatBackendOpenAI
	}
	return b
}

// ValidateChatBackend prevents a local compatibility dialect from changing
// paid-provider privacy contracts. It never infers a dialect from a URL/model.
func ValidateChatBackend(backend ChatBackend, endpoint, provider string, dataSharing bool) error {
	switch backend.Effective() {
	case ChatBackendOpenAI:
		return nil
	case ChatBackendOllama:
		if provider != "" || dataSharing {
			return fmt.Errorf("Ollama chat backend cannot use OpenRouter or OpenAI data sharing")
		}
	default:
		return fmt.Errorf("chat backend must be openai or ollama")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(endpoint, "#") ||
		u.Path != "/v1/chat/completions" || u.RawPath != "" {
		return fmt.Errorf("Ollama chat backend requires a plain /v1/chat/completions URL")
	}
	hostname := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if hostname == "api.openai.com" || hostname == "openrouter.ai" {
		return fmt.Errorf("Ollama chat backend cannot use a direct OpenAI or OpenRouter host")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (hostname == "127.0.0.1" || hostname == "localhost" || hostname == "::1")) {
		return fmt.Errorf("Ollama chat backend requires HTTPS (HTTP only on loopback)")
	}
	return nil
}
