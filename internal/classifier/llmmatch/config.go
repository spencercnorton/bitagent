// Package llmmatch implements the two-stage LLM-assisted TMDB matcher used as
// a fallback in the classifier workflow. It runs ONLY for torrents that a CEL
// rule already typed as movie/tv_show but that no deterministic step (local
// search, TMDB search) could attach to a real content id. Those torrents are
// invisible to Prowlarr/Sonarr/Radarr (they can only be grabbed by id or by a
// clean title+year), so recovering them is the whole point of the crawler.
//
// Two stages, both against an OpenAI-compatible chat endpoint (default: the
// on-net qwen3.6:35b — free, keeps metadata on the tailnet):
//
//  1. Extract — turn a messy release name into {title, year, type, season,
//     episode}, normalising foreign/alternate titles to the canonical one.
//  2. Rerank — given the TMDB search candidates for that title, pick the ONE
//     correct tmdb_id (or none). This is the precision gate: a wrong match is
//     worse than no match, so the model is told to return 0 when unsure.
//
// Two-flag opt-in mirrors llmstage: Enabled turns the stage on (shadow —
// computes the would-be match, emits metrics, attaches nothing); EnableLive
// promotes it to actually attach. Both default false.
package llmmatch

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/spencercnorton/bitagent/internal/llmprovider"
)

// Config controls the LLM matcher. Registered as "classifier_llm_match" on
// configfx; env prefix CLASSIFIER_LLM_MATCH_*.
type Config struct {
	// Enabled turns the stage on in shadow mode.
	Enabled bool `yaml:"enabled"`
	// EnableLive promotes shadow -> live (the chosen match is attached).
	EnableLive bool `yaml:"enable_live"`

	// APIKey / Model / Endpoint for the OpenAI-compatible chat API.
	// Default endpoint is the on-net qwen; APIKey may be empty for a
	// local ollama endpoint that ignores auth.
	APIKey   string `yaml:"api_key"`
	Model    string `yaml:"model"`
	Endpoint string `yaml:"endpoint"`
	// OpenRouter requires one explicit provider, ZDR and no fallback. Empty
	// retains the direct/local API contract; it cannot call OpenRouter.
	// Deliberate casing: the env resolver tokenizes Go field names, not YAML
	// tags. OpenRouterProvider would bind OPEN_ROUTER_PROVIDER instead.
	OpenrouterProvider string `yaml:"openrouter_provider"`
	// OpenaiDataSharing asserts that this direct OpenAI route belongs to the
	// opted-in BitAgent project. It selects a fail-closed request contract; the
	// provider usage receipt remains the billing proof.
	OpenaiDataSharing bool `yaml:"openai_data_sharing"`

	// PromptVersion is part of the cache key — bump to invalidate.
	PromptVersion string `yaml:"prompt_version"`

	Timeout   time.Duration `yaml:"timeout"`
	CacheSize int           `yaml:"cache_size"`
	// Every outbound attempt (including failed calls and batch retries) consumes
	// one slot. Production stores the allowance in Postgres across restarts.
	// Zero stops outbound calls; it never means unlimited.
	DailyCallLimit     int `yaml:"daily_call_limit"`
	MonthlyCallLimit   int `yaml:"monthly_call_limit"`
	MaxRequestBytes    int `yaml:"max_request_bytes"`
	MaxOutputTokens    int `yaml:"max_output_tokens"`
	MaxConcurrentCalls int `yaml:"max_concurrent_calls"`
	// RequireSourceTitle additionally anchors candidate identity to the title
	// parsed without an LLM. Use for the initial live canary; model agreement
	// alone cannot establish that an invented title belongs to a release.
	RequireSourceTitle bool `yaml:"require_source_title"`

	// MaxCandidates caps how many TMDB search hits are shown to the
	// rerank call.
	MaxCandidates int `yaml:"max_candidates"`

	// MinConfidence is the floor a live-mode rerank must clear to
	// attach. Shadow metrics still record lower-confidence decisions.
	MinConfidence float64 `yaml:"min_confidence"`

	// Plausibility gate bounds (same intent as llmstage).
	MinTotalSizeBytes int64 `yaml:"min_total_size_bytes"`
	MaxTotalSizeBytes int64 `yaml:"max_total_size_bytes"`
	MaxFiles          int   `yaml:"max_files"`

	// AnimeRequireEnglish drops anime with no English audio or subtitle
	// track (a "raw"). We only want anime we can actually watch in English.
	AnimeRequireEnglish bool `yaml:"anime_require_english"`
	// AnimeAllowSubOnly accepts English-subtitled anime (Japanese audio).
	// When false, only English dubs pass. Default true — English subs count
	// as "an English track of some kind".
	AnimeAllowSubOnly bool `yaml:"anime_allow_sub_only"`
}

// NewDefaultConfig returns safe, inert defaults — both flags false, so
// including the module never calls out until an operator opts in.
func NewDefaultConfig() Config {
	return Config{
		Enabled:             false,
		EnableLive:          false,
		Model:               "qwen3.6:35b",
		Endpoint:            "http://127.0.0.1:11434/v1/chat/completions",
		PromptVersion:       "v4-2026-07-19-dual-audio-english",
		Timeout:             45 * time.Second,
		CacheSize:           20000,
		DailyCallLimit:      100,
		MonthlyCallLimit:    3000,
		MaxRequestBytes:     8 * 1024,
		MaxOutputTokens:     8192, // offline grouped extraction; live canary pins 256
		MaxConcurrentCalls:  2,
		MaxCandidates:       6,
		MinConfidence:       0.75,
		MinTotalSizeBytes:   50 * 1024 * 1024,         // 50 MB
		MaxTotalSizeBytes:   200 * 1024 * 1024 * 1024, // 200 GB
		MaxFiles:            2000,
		AnimeRequireEnglish: true,
		AnimeAllowSubOnly:   true,
	}
}

// Validate rejects unsafe live decision thresholds at startup. The attach
// boundary still defends itself because direct library callers can bypass the
// fx wiring, but an operator typo must not silently turn into a matcher that
// merely stops attaching (or attaches everything when the value is NaN).
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	endpoint, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("invalid matcher endpoint")
	}
	hostname := strings.TrimRight(strings.ToLower(endpoint.Hostname()), ".")
	if hostname == "openrouter.ai" || c.OpenrouterProvider != "" {
		if c.Endpoint != "https://openrouter.ai/api/v1/chat/completions" ||
			strings.TrimSpace(c.OpenrouterProvider) == "" || strings.TrimSpace(c.OpenrouterProvider) != c.OpenrouterProvider {
			return fmt.Errorf("OpenRouter requires its HTTPS chat endpoint and an explicit provider pin")
		}
	}
	if err := llmprovider.ValidateDataSharingChatEndpoint(
		c.OpenaiDataSharing, c.Endpoint, c.Model, c.APIKey, c.OpenrouterProvider,
	); err != nil {
		return fmt.Errorf("matcher: %w", err)
	}
	if c.DailyCallLimit < 0 || c.MonthlyCallLimit < 0 {
		return fmt.Errorf("call limits must be non-negative; zero disables calls")
	}
	if c.MaxRequestBytes < 1 || c.MaxOutputTokens < 1 || c.MaxConcurrentCalls < 1 || c.Timeout <= 0 {
		return fmt.Errorf("request size, concurrency and timeout must be positive")
	}
	if math.IsNaN(c.MinConfidence) || math.IsInf(c.MinConfidence, 0) ||
		c.MinConfidence <= 0 || c.MinConfidence > 1 {
		return fmt.Errorf("min_confidence must be finite and in (0,1]")
	}
	return nil
}
