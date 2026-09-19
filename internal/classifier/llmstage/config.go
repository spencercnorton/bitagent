// Package llmstage adds a fallback LLM classifier between the CEL
// runner and the canonical-label preempt layer. It is deliberately
// constrained:
//
//   - OFF by default at both Enabled and EnableLive flags.
//   - Shadow mode first (Enabled=true, EnableLive=false) — the stage
//     runs, emits metrics, but does not alter the classification
//     result. Operator promotes to live only after the precision
//     metric against canonical labels clears a threshold.
//   - Hard privacy gate — never sends torrent metadata originating
//     from qBittorrent instances tagged "private" / "bitgrab" or from
//     any other evidence row flagged private. Private-tracker content
//     does not leave this host.
//   - Plausibility gate — skips calls on payloads that obviously are
//     not media (no video/audio files, extreme file counts, tiny or
//     huge total size).
//   - Hit-cache with sha256 keys including prompt_version so a prompt
//     change invalidates the cache with a single bump.
//
// The promotion gate (precision/recall vs later canonical labels) is
// observed externally via the emitted metrics; flipping EnableLive
// is a manual operator action.
package llmstage

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/spencercnorton/bitagent/internal/llmprovider"
)

// Config controls the LLM classifier stage.
//
// Registered as "classifier_llm" on configfx. Env prefix
// CLASSIFIER_LLM_*.
type Config struct {
	// Enabled turns the stage on. When false, the decorator is
	// inert regardless of other flags.
	Enabled bool `yaml:"enabled"`

	// EnableLive switches the stage from shadow mode to live.
	// Shadow mode: LLM runs, decision is logged as a metric, the
	// inner CEL result is returned unchanged.
	// Live mode: the LLM decision replaces inner on match, leaves
	// inner on no-match.
	EnableLive bool `yaml:"enable_live"`

	// APIKey for OpenAI. Populated from Infisical at deploy.
	APIKey string `yaml:"api_key"`

	// Model is the OpenAI model to call. Default gpt-5.4-nano for cost.
	Model string `yaml:"model"`

	// PromptVersion bumps whenever the prompt template changes. It
	// is part of the cache key, so bumping invalidates all cached
	// decisions in one step. Do not reuse a previously published
	// value.
	PromptVersion string `yaml:"prompt_version"`

	// Endpoint override for the OpenAI chat completions API. Blank
	// uses the public endpoint.
	Endpoint string `yaml:"endpoint"`

	// OpenRouter requires one explicit provider, ZDR and no fallback. Empty
	// retains the direct/local API contract; it cannot call OpenRouter.
	// Deliberate casing: the env resolver tokenizes Go field names, not YAML
	// tags. OpenRouterProvider would bind OPEN_ROUTER_PROVIDER instead.
	OpenrouterProvider string `yaml:"openrouter_provider"`
	// OpenaiDataSharing selects the strict direct-OpenAI incentive contract.
	// The API usage receipt, not this assertion alone, proves complimentary use.
	OpenaiDataSharing bool `yaml:"openai_data_sharing"`

	// Timeout bounds a single LLM call including network and retry.
	Timeout time.Duration `yaml:"timeout"`

	// CacheSize caps the in-memory LRU of decisions. Restart wipes
	// the cache; that is fine — LLM calls are expected to be rare
	// once the evidence ingestor has coverage.
	CacheSize int `yaml:"cache_size"`

	// MinTotalSizeBytes rejects torrents below this size (probably
	// not a real media payload).
	MinTotalSizeBytes int64 `yaml:"min_total_size_bytes"`

	// MaxTotalSizeBytes rejects torrents above this size (often
	// linux distros / data dumps; rarely classifiable from title).
	MaxTotalSizeBytes int64 `yaml:"max_total_size_bytes"`

	// MaxFiles rejects torrents with absurd file counts.
	MaxFiles int `yaml:"max_files"`

	// MinConfidence is the threshold a live-mode result must meet
	// to be applied. Below this, the stage returns the inner
	// result even in live mode. Shadow metrics still record the
	// low-confidence decision.
	MinConfidence float64 `yaml:"min_confidence"`

	// Zero denies all calls. Reservations survive restarts and are separate
	// from the catalogue matcher's allowance; failed requests are not refunded.
	DailyCallLimit     int `yaml:"daily_call_limit"`
	MonthlyCallLimit   int `yaml:"monthly_call_limit"`
	MaxConcurrentCalls int `yaml:"max_concurrent_calls"`
	MaxRequestBytes    int `yaml:"max_request_bytes"`
	MaxOutputTokens    int `yaml:"max_output_tokens"`
}

// NewDefaultConfig returns safe defaults. Both Enabled and
// EnableLive are false — a deploy that does not override these will
// never call OpenAI.
func NewDefaultConfig() Config {
	return Config{
		Enabled:            false,
		EnableLive:         false,
		Model:              "gpt-5.4-nano",
		PromptVersion:      "v1-2026-04-21",
		Endpoint:           "https://api.openai.com/v1/chat/completions",
		Timeout:            30 * time.Second,
		CacheSize:          10000,
		MinTotalSizeBytes:  50 * 1024 * 1024,         // 50 MB
		MaxTotalSizeBytes:  200 * 1024 * 1024 * 1024, // 200 GB
		MaxFiles:           2000,
		MinConfidence:      0.75,
		DailyCallLimit:     50,
		MonthlyCallLimit:   1500,
		MaxConcurrentCalls: 1,
		MaxRequestBytes:    8192,
		MaxOutputTokens:    64,
	}
}

// Validate is repeated at the outbound boundary for standalone callers.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) {
		return fmt.Errorf("classifier_llm: endpoint must be credential-free HTTPS (HTTP only on loopback)")
	}
	hostname := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if hostname == "openrouter.ai" || c.OpenrouterProvider != "" {
		if c.Endpoint != "https://openrouter.ai/api/v1/chat/completions" ||
			strings.TrimSpace(c.OpenrouterProvider) == "" || strings.TrimSpace(c.OpenrouterProvider) != c.OpenrouterProvider {
			return fmt.Errorf("classifier_llm: OpenRouter requires its HTTPS chat endpoint and an explicit provider pin")
		}
	}
	if err := llmprovider.ValidateDataSharingChatEndpoint(
		c.OpenaiDataSharing, c.Endpoint, c.Model, c.APIKey, c.OpenrouterProvider,
	); err != nil {
		return fmt.Errorf("classifier_llm: %w", err)
	}
	if strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.Model) == "" || strings.TrimSpace(c.PromptVersion) == "" {
		return fmt.Errorf("classifier_llm: key, model and prompt version are required")
	}
	if c.Timeout <= 0 || c.Timeout > time.Minute || c.CacheSize <= 0 || c.CacheSize > 100000 ||
		c.MinTotalSizeBytes < 0 || c.MaxTotalSizeBytes <= c.MinTotalSizeBytes || c.MaxFiles <= 0 ||
		c.MaxConcurrentCalls <= 0 || c.MaxConcurrentCalls > 16 || c.MaxRequestBytes <= 0 || c.MaxRequestBytes > 64<<10 ||
		c.MaxOutputTokens <= 0 || c.MaxOutputTokens > 1024 || c.DailyCallLimit < 0 || c.MonthlyCallLimit < 0 ||
		math.IsNaN(c.MinConfidence) || math.IsInf(c.MinConfidence, 0) || c.MinConfidence <= 0 || c.MinConfidence > 1 {
		return fmt.Errorf("classifier_llm: invalid bounded admission configuration")
	}
	return nil
}
