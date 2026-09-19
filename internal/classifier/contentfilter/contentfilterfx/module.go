// Package contentfilterfx wires the content filter as an fx-provided
// `*contentfilter.Filter` + `*contentfilter.Metrics`, so any consumer
// that needs to call Decide() / DecideDeterministic() can inject the
// SAME filter instance — sharing the LLM cache, daily budget, and
// rule miner across consumers.
//
// Two production consumers:
//
//   - the dhtcrawler runs DecideDeterministic at the BEP-9 success
//     hook (pre-classifier, no language tag yet)
//   - the post-classifier hook in internal/processor runs Decide on
//     the surviving torrents AFTER the CEL classifier has populated
//     ContentType + Languages, which is what enables the LLM tier
//     on the residual cohort
//
// Putting the filter in its own fx module also gets the prometheus
// collectors registered exactly once (collectors() is flattened into
// the prometheus_collectors fx group) regardless of how many
// consumers pull the filter — Prometheus would panic on a duplicate
// registration if each consumer wired its own metrics instance.
package contentfilterfx

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmprovider"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// New wires the contentfilter module. Provides:
//
//   - `*contentfilter.Filter` (deterministic-only, or LLM-attached
//     when the operator's config asks for it and a key/base URL is
//     present)
//   - `*contentfilter.Metrics`
//   - the metric collectors flattened into the prometheus_collectors
//     fx group
//
// Also registers the `content_filter` config section. Previously
// dhtcrawlerfx owned this registration, but moving it here lets a
// reduced-scope build (e.g. a future processor-only test or a CLI
// tool that doesn't pull the full DHT crawler) still get the filter.
func New() fx.Option {
	return fx.Module(
		"content_filter",
		// Default Enabled=false so a fresh deploy is a pure no-op
		// until the operator opts in via env:
		//
		//   CONTENT_FILTER_ENABLED=true   # examine + shadow metrics
		//   CONTENT_FILTER_ENFORCE=true   # actually drop matched torrents
		//
		// Two-stage opt-in mirrors retention's Enabled/EnablePurge
		// and peerrep's Enabled/Enforce.
		configfx.NewConfigModule[contentfilter.Config]("content_filter", contentfilter.NewDefaultConfig()),
		fx.Provide(
			contentfilter.NewMetrics,
			provideFilter,
			fx.Annotated{
				Group: "prometheus_collectors,flatten",
				Target: func(m *contentfilter.Metrics) []prometheus.Collector {
					return m.Collectors()
				},
			},
		),
	)
}

// provideFilter chooses between the deterministic-only constructor
// and the LLM-attached one based on operator config.
//
// Production preconditions for the LLM tier:
//
//  1. cfg.LLMEnabled == true (operator opt-in flag)
//  2. the exact authenticated, provider-pinned OpenRouter contract validates
//
// A disabled tier uses the deterministic-only path. An enabled but malformed
// production route fails startup so it cannot silently fall back to an
// unpinned or unaudited provider.
//
// (Moved from internal/dhtcrawler/factory.go::buildContentFilter so
// any consumer of *contentfilter.Filter sees the same instance.)
type filterParams struct {
	fx.In
	Config  contentfilter.Config
	Metrics *contentfilter.Metrics
	Logger  *zap.SugaredLogger
	Capture llmcapture.Capturer `optional:"true"`
	Pool    lazy.Lazy[*pgxpool.Pool]
}

func provideFilter(p filterParams) (*contentfilter.Filter, error) {
	cfg, m, logger := p.Config, p.Metrics, p.Logger
	cfg = applyDropLossyAudioOnlyBackCompat(cfg, logger)

	if !cfg.LLMEnabled {
		return contentfilter.New(cfg), nil
	}
	if err := validateProductionLLMConfig(cfg); err != nil {
		return nil, err
	}
	if p.Capture == nil || !p.Capture.Enabled() {
		return nil, fmt.Errorf("content_filter LLM requires enabled durable evaluation capture")
	}

	timeout, err := time.ParseDuration(cfg.LLMTimeout)
	if err != nil || timeout <= 0 {
		timeout = 8 * time.Second
	}
	llm := contentfilter.NewOpenAIClientWithPolicy(
		cfg.LLMOpenaiApiKey,
		cfg.LLMModel,
		cfg.LLMBaseURL,
		cfg.LLMApiStyle,
		cfg.LLMPromptVersion,
		cfg.LLMOpenrouterProvider,
		cfg.LLMMaxOutputTokens,
		timeout,
		cfg.LLMOpenaiDataSharing,
	)
	cb := m.LLMCallbacks()
	apiStyle := cfg.LLMApiStyle
	if apiStyle == "" {
		apiStyle = "auto"
	}
	logger.Named("contentfilter").Infow(
		"LLM tier active",
		"model", cfg.LLMModel,
		"base_url_set", cfg.LLMBaseURL != "",
		"api_style", apiStyle,
		"daily_budget", cfg.LLMDailyBudget,
		"monthly_budget", cfg.LLMMonthlyBudget,
		"max_concurrent_calls", cfg.LLMMaxConcurrentCalls,
		"defer_on_unavailable", cfg.LLMDeferOnUnavailable,
		"min_confidence_for_drop", cfg.LLMMinConfidenceForDrop,
		"prompt_version", cfg.LLMPromptVersion,
	)
	return contentfilter.NewWithLLMAdmission(cfg, llm, cb, contentfilter.Admission{
		Budget:  llmmatch.NewPostgresContentFilterCallBudget(p.Pool),
		Capture: p.Capture,
	}), nil
}

func validateProductionLLMConfig(cfg contentfilter.Config) error {
	if math.IsNaN(cfg.LLMMinConfidenceForDrop) ||
		math.IsInf(cfg.LLMMinConfidenceForDrop, 0) ||
		cfg.LLMMinConfidenceForDrop <= 0 ||
		cfg.LLMMinConfidenceForDrop > 1 {
		return fmt.Errorf("content_filter.llm_min_confidence_for_drop must be finite and in (0,1]")
	}
	if cfg.LLMDailyBudget <= 0 || cfg.LLMMonthlyBudget <= 0 ||
		cfg.LLMMonthlyBudget < cfg.LLMDailyBudget {
		return fmt.Errorf("content_filter LLM daily/monthly budgets must be positive and monthly >= daily")
	}
	if cfg.LLMMaxConcurrentCalls <= 0 || cfg.LLMMaxConcurrentCalls > 16 ||
		cfg.LLMMaxRequestBytes <= 0 || cfg.LLMMaxRequestBytes > 1<<20 ||
		cfg.LLMMaxOutputTokens <= 0 || cfg.LLMMaxOutputTokens > 4096 {
		return fmt.Errorf("content_filter LLM concurrency/request/output bounds are invalid")
	}
	if cfg.LLMOpenaiDataSharing {
		if err := llmprovider.ValidateDataSharingChatBaseURL(
			true, cfg.LLMBaseURL, cfg.LLMApiStyle, cfg.LLMModel,
			cfg.LLMOpenaiApiKey, cfg.LLMOpenrouterProvider,
		); err != nil {
			return fmt.Errorf("content_filter: %w", err)
		}
		return nil
	}
	if cfg.LLMBaseURL != "https://openrouter.ai/api/v1" ||
		strings.TrimSpace(cfg.LLMOpenrouterProvider) == "" ||
		cfg.LLMApiStyle != "chat" || cfg.LLMOpenaiApiKey == "" {
		return fmt.Errorf("content_filter production LLM requires an exact pinned OpenRouter route or the OpenAI data-sharing route")
	}
	return nil
}

// applyDropLossyAudioOnlyBackCompat honors the legacy
// CONTENT_FILTER_DROP_MP_3_ONLY env var (the pre-rename binding for what
// is now DropLossyAudioOnly / CONTENT_FILTER_DROP_LOSSY_AUDIO_ONLY) so a
// running deploy that still sets the old name doesn't silently change
// behavior on upgrade.
//
// Precedence: the new env var always wins. We only fall back to the
// legacy var when the NEW one is unset (the normal config resolver has
// already applied the new var to cfg if present). When the legacy var is
// the only one set, we override cfg.DropLossyAudioOnly with its value and
// log a one-time deprecation WARN pointing at the new name.
func applyDropLossyAudioOnlyBackCompat(cfg contentfilter.Config, logger *zap.SugaredLogger) contentfilter.Config {
	const (
		newKey = "CONTENT_FILTER_DROP_LOSSY_AUDIO_ONLY"
		oldKey = "CONTENT_FILTER_DROP_MP_3_ONLY"
	)
	if _, newSet := os.LookupEnv(newKey); newSet {
		return cfg // new var present — resolver already applied it; ignore legacy.
	}
	legacyVal, oldSet := os.LookupEnv(oldKey)
	if !oldSet {
		return cfg
	}
	parsed, err := strconv.ParseBool(legacyVal)
	if err != nil {
		logger.Named("contentfilter").Warnw(
			"deprecated env "+oldKey+" set to an unparseable bool; ignoring — "+
				"please migrate to "+newKey,
			"value", legacyVal)
		return cfg
	}
	cfg.DropLossyAudioOnly = parsed
	logger.Named("contentfilter").Warnw(
		"deprecated env "+oldKey+" is set; honoring it for backward compatibility. "+
			"Rename it to "+newKey+" — the old name will be removed in a future release.",
		"drop_lossy_audio_only", parsed)
	return cfg
}
