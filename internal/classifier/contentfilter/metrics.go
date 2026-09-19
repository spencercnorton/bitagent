package contentfilter

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics is the contentfilter telemetry surface. All counters and
// histograms are dual-emitted (bitmagnet_* legacy name + bitagent_*
// canonical name) so existing dashboards keep working through the
// namespace migration.
//
// Phase 1 (deterministic ladder):
//
//	examined_total           — every torrent the filter saw
//	keep_total               — torrents that passed every check
//	drop_total{reason}       — torrents the filter dropped (Enforce=true)
//	would_drop_total{reason} — counterfactual: dropped under Enforce=false
//
// Phase 2 (LLM tier):
//
//	llm_cache_hits_total
//	llm_cache_misses_total
//	llm_calls_total{ok}
//	llm_call_duration_seconds (histogram)
//	llm_budget_exhausted_total
//	llm_rule_candidate_total{reason,is_english}
//
// The Phase 2 collectors are always created; if LLMEnabled=false
// they simply never increment. That keeps the Collectors() signature
// stable and lets the operator flip the LLM tier on without a
// dashboard update.
type Metrics struct {
	examined  *dualemit.Counter
	kept      *dualemit.Counter
	dropped   *dualemit.CounterVec
	wouldDrop *dualemit.CounterVec

	// blocked_ext_total{ext="..."} — sub-breakdown of drop_total where
	// reason="blocked_extension". Additive to drop_total; lets the
	// operator see which extensions actually fire so they can tune
	// the BlockedExtensions list. Only emitted when reason matches.
	blockedExt *dualemit.CounterVec

	// Phase 2 — LLM tier.
	llmCacheHits       *dualemit.Counter
	llmCacheMisses     *dualemit.Counter
	llmCalls           *dualemit.CounterVec // ok=true|false
	llmTokens          *dualemit.CounterVec // model, kind=input|output|cached_input|reasoning
	llmUsageMissing    *dualemit.CounterVec // model
	llmGateRejects     *dualemit.CounterVec // reason
	llmAudit           *dualemit.CounterVec // outcome
	llmCallDuration    *dualemit.Histogram
	llmBudgetExhausted *dualemit.Counter
	llmRuleCandidate   *dualemit.CounterVec // reason, is_english

	// llm_deferred_total — residual torrents re-queued (not kept, not
	// dropped) because the LLM endpoint was unreachable and
	// LLMDeferOnUnavailable is set. Climbs while a self-hosted LLM is
	// offline; flat at zero in the default (fail-open-keep) config.
	llmDeferred *dualemit.Counter
}

const (
	cfNamespace = "bitagent"
	cfSubsystem = "contentfilter"
	cfLabel     = "reason"
)

// NewMetrics builds the metric set. Call Collectors() to register
// with the prometheus_collectors fx group.
func NewMetrics() *Metrics {
	return &Metrics{
		examined: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "examined_total",
			Help:      "Torrents the content filter examined (post-classify, pre-persist).",
		}),
		kept: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "keep_total",
			Help:      "Torrents the content filter passed without dropping.",
		}),
		dropped: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "drop_total",
			Help:      "Torrents dropped by the content filter (Enforce=true). Per-reason.",
		}, []string{cfLabel}),
		wouldDrop: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "would_drop_total",
			Help:      "Counterfactual: torrents the filter would drop under Enforce=true. Per-reason. Lights up in shadow mode (Enabled=true, Enforce=false).",
		}, []string{cfLabel}),

		blockedExt: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "blocked_ext_total",
			Help:      "Torrents dropped because their primary extension is on the blocklist. Sub-breakdown of drop_total{reason=blocked_extension}. Use to tune BlockedExtensions.",
		}, []string{"ext"}),

		// Phase 2 — LLM tier.
		llmCacheHits: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_cache_hits_total",
			Help:      "LLM verdicts served from the sha256-keyed LRU cache.",
		}),
		llmCacheMisses: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_cache_misses_total",
			Help:      "LLM verdicts NOT in cache (a live LLM call follows, subject to budget).",
		}),
		llmCalls: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_calls_total",
			Help:      "Live LLM calls made by the filter. ok=true on success, ok=false on error/timeout.",
		}, []string{"ok"}),
		llmTokens: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_tokens_total",
			Help: "Tokens reported by the provider for live LLM calls, by model. " +
				"kind=input|output|cached_input|reasoning. This is the ONLY token accounting bitagent " +
				"emits; cost is derived downstream where the per-model price table lives.",
		}, []string{"model", "kind"}),
		llmUsageMissing: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_usage_missing_total",
			Help:      "Provider completions without usable token accounting; any increase makes efficiency totals incomplete.",
		}, []string{"model"}),
		llmGateRejects: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_gate_rejects_total",
			Help:      "Content-filter LLM dispatches rejected before provider egress, by bounded reason.",
		}, []string{"reason"}),
		llmAudit: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_audit_total",
			Help:      "Durable content-filter LLM result and policy-decision audit outcomes.",
		}, []string{"outcome"}),
		llmCallDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_call_duration_seconds",
			Help:      "Wall-clock latency of live LLM calls.",
			// Buckets cover gpt-5.4-nano typical (200-800ms) plus
			// the 8s timeout ceiling.
			Buckets: []float64{0.05, 0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4, 8.0},
		}),
		llmBudgetExhausted: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_budget_exhausted_total",
			Help:      "LLM consultations that hit the daily budget cap and fell through to keep.",
		}),
		llmRuleCandidate: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_rule_candidate_total",
			Help:      "A (reason, is_english) tuple just crossed the rule-miner threshold and is a candidate for codification as a deterministic rule.",
		}, []string{"reason", "is_english"}),
		llmDeferred: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "llm_deferred_total",
			Help:      "Residual torrents re-queued (not kept, not dropped) because the LLM endpoint was unreachable and defer-on-unavailable is enabled.",
		}),
	}
}

// Collectors returns every collector for fx-group registration.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.examined, m.kept, m.dropped, m.wouldDrop, m.blockedExt,
		m.llmCacheHits, m.llmCacheMisses, m.llmCalls, m.llmTokens, m.llmUsageMissing,
		m.llmGateRejects, m.llmAudit,
		m.llmCallDuration, m.llmBudgetExhausted, m.llmRuleCandidate,
		m.llmDeferred,
	}
}

// Observe records the outcome of a Decide() call. Caller should
// invoke this exactly once per torrent decision.
func (m *Metrics) Observe(d Decision) {
	m.examined.Inc()
	if d.Defer {
		// Re-queued: neither kept nor dropped. Count separately so
		// keep/drop rates aren't skewed during an LLM outage.
		m.llmDeferred.Inc()
		return
	}
	switch {
	case d.WouldDrop && !d.Allow:
		// Live drop: increment the dropped counter for the reason.
		m.dropped.With(prometheus.Labels{cfLabel: d.Reason.String()}).Inc()
		if d.Reason == ReasonBlockedExtension && d.BlockedExt != "" {
			m.blockedExt.With(prometheus.Labels{"ext": d.BlockedExt}).Inc()
		}
	case d.WouldDrop && d.Allow:
		// Shadow mode: counterfactual visible only.
		m.wouldDrop.With(prometheus.Labels{cfLabel: d.Reason.String()}).Inc()
		if d.Reason == ReasonBlockedExtension && d.BlockedExt != "" {
			m.blockedExt.With(prometheus.Labels{"ext": d.BlockedExt}).Inc()
		}
		m.kept.Inc() // it was kept (shadow mode keeps everything)
	default:
		m.kept.Inc()
	}
}

// LLMCallbacks returns a callbacks struct wired to this Metrics
// instance. Pass the result to NewWithLLM. Hooks are called from
// the BEP-9 fetcher's hot path so they MUST be lock-free —
// dualemit's atomic counters satisfy that.
func (m *Metrics) LLMCallbacks() LLMCallbacks {
	return LLMCallbacks{
		OnCacheHit:  func() { m.llmCacheHits.Inc() },
		OnCacheMiss: func() { m.llmCacheMisses.Inc() },
		OnLLMCall: func(ok bool, latencyMs float64) {
			label := "true"
			if !ok {
				label = "false"
			}
			m.llmCalls.With(prometheus.Labels{"ok": label}).Inc()
			m.llmCallDuration.Observe(latencyMs / 1000.0)
		},
		OnLLMTokens: func(model string, usage TokenUsage) {
			// A provider that reports nothing must not be recorded as zero
			// tokens — that reads as "this call was free" on the dashboard,
			// which is exactly the failure mode this metric exists to end.
			if model == "" || !usage.Valid() {
				return
			}
			for kind, value := range map[string]int{
				"input": usage.PromptTokens, "output": usage.CompletionTokens,
				"cached_input": usage.CachedInputTokens, "reasoning": usage.ReasoningTokens,
			} {
				m.llmTokens.With(prometheus.Labels{"model": model, "kind": kind}).Add(float64(value))
			}
		},
		OnLLMUsageMissing: func(model string) {
			if model != "" {
				m.llmUsageMissing.With(prometheus.Labels{"model": model}).Inc()
			}
		},
		OnGateReject: func(reason string) {
			m.llmGateRejects.With(prometheus.Labels{"reason": reason}).Inc()
		},
		OnAudit: func(outcome string) {
			m.llmAudit.With(prometheus.Labels{"outcome": outcome}).Inc()
		},
		OnBudgetExhausted: func() { m.llmBudgetExhausted.Inc() },
		OnRuleCandidate: func(reason string, isEnglish bool, _ int) {
			eng := "false"
			if isEnglish {
				eng = "true"
			}
			m.llmRuleCandidate.With(prometheus.Labels{
				"reason":     reason,
				"is_english": eng,
			}).Inc()
		},
	}
}
