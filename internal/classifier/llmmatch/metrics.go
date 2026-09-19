package llmmatch

import "github.com/prometheus/client_golang/prometheus"

// Metrics observes the matcher. Natural provider-backed decisions also have a
// bounded capture/result ledger; aggregate counters alone are not a precision
// evaluation cohort.
type Metrics struct {
	extractTotal *prometheus.CounterVec // result=ok|empty|error
	rerankTotal  *prometheus.CounterVec // result=match|none|error
	// matchesTotal: matches passing all final attachment guards. mode=shadow|live,
	// media_type=movie|tv.
	matchesTotal *prometheus.CounterVec
	// gate=size|files|plausibility|privacy|pack|adult|candidate_title|
	//      candidate_ambiguous|candidate_year|resolved_year
	// resolved_year fires AFTER content resolution, when the real release year
	// contradicts the name and the pre-attach candidate year was unknown.
	gateRejects *prometheus.CounterVec
	// candidatesTotal: rerank candidate sets built, by source — "local" is a
	// free hit on the mirrored content table; "api" is a TMDB search call.
	candidatesTotal *prometheus.CounterVec
	// animeTotal: anime observed, by english track (dub|sub|none|unknown)
	// and outcome (kept|rejected on the English gate).
	animeTotal     *prometheus.CounterVec
	cacheHits      prometheus.Counter
	cacheMisses    prometheus.Counter
	callErrors     *prometheus.CounterVec // stage=extract|rerank, class=timeout|http_status|decode
	callDuration   *prometheus.HistogramVec
	calls          *prometheus.CounterVec
	tokens         *prometheus.CounterVec
	usageMissing   *prometheus.CounterVec
	budgetSkips    *prometheus.CounterVec
	config         *prometheus.GaugeVec
	info           *prometheus.GaugeVec
	auditResults   *prometheus.CounterVec
	auditDecisions *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	return &Metrics{
		auditResults:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_classifier_llm_match_audit_results_total", Help: "First provider response capture outcomes, by stage. Cache hits are outside this cohort."}, []string{"stage", "outcome"}),
		auditDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_classifier_llm_match_audit_decisions_total", Help: "Final policy decision recording outcomes. No-provider and duplicate paths are not new cohort observations."}, []string{"outcome"}),
		info:           prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bitagent_classifier_llm_match_info", Help: "Configured matcher model and prompt version; present even before any call."}, []string{"model", "prompt_version"}),
		calls:          prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_classifier_llm_match_calls_total", Help: "Outbound matcher requests admitted by the durable allowance, by model and stage."}, []string{"model", "stage"}),
		tokens:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_classifier_llm_match_tokens_total", Help: "Provider-reported matcher tokens. cached_input is a subset of input; reasoning is a subset of output."}, []string{"model", "stage", "kind"}),
		usageMissing:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_classifier_llm_match_usage_missing_total", Help: "Matcher HTTP responses without valid provider usage; spend estimates are incomplete."}, []string{"model", "stage"}),
		budgetSkips:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_classifier_llm_match_budget_skips_total", Help: "Calls withheld because the durable request allowance is exhausted or unavailable."}, []string{"reason"}),
		config:         prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bitagent_classifier_llm_match_config", Help: "Resolved matcher configuration, emitted even before the first call. live means enabled and enable_live."}, []string{"setting"}),
		extractTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bitagent_classifier_llm_match_extract_total",
			Help: "LLM matcher stage-1 (extract) calls by result.",
		}, []string{"result"}),
		rerankTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bitagent_classifier_llm_match_rerank_total",
			Help: "LLM matcher stage-2 (rerank) calls by result.",
		}, []string{"result"}),
		matchesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bitagent_classifier_llm_match_matches_total",
			Help: "Matcher decisions passing all final attachment guards, by mode and media type. Shadow suppresses attachment and tagging.",
		}, []string{"mode", "media_type"}),
		gateRejects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bitagent_classifier_llm_match_gate_rejects_total",
			Help: "Torrents rejected by a matcher gate (pre-LLM plausibility/privacy, post-extract pack/adult, or post-rerank identity).",
		}, []string{"gate"}),
		candidatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bitagent_classifier_llm_match_candidates_total",
			Help: "Rerank candidate sets built, by source (local content mirror vs tmdb api).",
		}, []string{"source"}),
		animeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bitagent_classifier_llm_match_anime_total",
			Help: "Anime observed by the matcher, by english track and outcome.",
		}, []string{"english", "outcome"}),
		cacheHits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "bitagent_classifier_llm_match_cache_hits_total",
			Help: "LLM matcher decision cache hits.",
		}),
		cacheMisses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "bitagent_classifier_llm_match_cache_misses_total",
			Help: "LLM matcher decision cache misses.",
		}),
		callErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bitagent_classifier_llm_match_call_errors_total",
			Help: "LLM matcher call errors, by stage and class.",
		}, []string{"stage", "class"}),
		callDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "bitagent_classifier_llm_match_call_duration_seconds",
			Help:    "LLM matcher call latency, by stage.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
		}, []string{"stage"}),
	}
}

// Collectors exposes the collectors for the shared prometheus registry.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.extractTotal, m.rerankTotal, m.matchesTotal, m.gateRejects, m.candidatesTotal, m.animeTotal,
		m.cacheHits, m.cacheMisses, m.callErrors, m.callDuration,
		m.calls, m.tokens, m.usageMissing, m.budgetSkips, m.config, m.info, m.auditResults, m.auditDecisions,
	}
}

func (m *Metrics) configure(c Config) {
	m.info.WithLabelValues(c.Model, c.PromptVersion).Set(1)
	flag := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	for setting, value := range map[string]float64{
		"enabled": flag(c.Enabled), "live": flag(c.Enabled && c.EnableLive),
		"require_source_title": flag(c.RequireSourceTitle),
		"daily_call_limit":     float64(c.DailyCallLimit), "monthly_call_limit": float64(c.MonthlyCallLimit),
		"max_request_bytes": float64(c.MaxRequestBytes), "max_concurrent_calls": float64(c.MaxConcurrentCalls),
		"max_output_tokens": float64(c.MaxOutputTokens),
		"min_confidence":    c.MinConfidence,
	} {
		m.config.WithLabelValues(setting).Set(value)
	}
}
