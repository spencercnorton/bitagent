package llmstage

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics cover the gates, the cache, the LLM call itself, and the
// shadow-mode agreement signal used as the promotion gate.
type Metrics struct {
	invocationsTotal   *dualemit.CounterVec
	gateRejectsTotal   *dualemit.CounterVec
	cacheHitsTotal     *dualemit.Counter
	cacheMissesTotal   *dualemit.Counter
	callDuration       *dualemit.Histogram
	callErrorsTotal    *dualemit.CounterVec
	decisionsTotal     *dualemit.CounterVec // shadow + live
	liveAppliedTotal   *dualemit.CounterVec
	shadowSkippedTotal *dualemit.Counter
	callsTotal         *dualemit.CounterVec
	tokensTotal        *dualemit.CounterVec
	usageMissingTotal  *dualemit.CounterVec
	auditTotal         *dualemit.CounterVec
	config             *dualemit.GaugeVec
}

func NewMetrics() *Metrics {
	const (
		namespace = "bitagent"
		subsystem = "classifier_llm"
	)
	m := &Metrics{
		config: dualemit.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "config",
			Help: "Effective bounded type-stage configuration; never includes credentials.",
		}, []string{"setting"}),
		callsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "calls_total",
			Help: "Actual type-classification HTTP dispatches, excluding cache and admission rejection.",
		}, []string{"model"}),
		tokensTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "tokens_total",
			Help: "Provider-reported type-classification tokens. cached_input is a subset of input; reasoning is a subset of output.",
		}, []string{"model", "kind"}),
		usageMissingTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "usage_missing_total",
			Help: "Type-classification HTTP responses without valid provider usage; spend estimates are incomplete.",
		}, []string{"model"}),
		auditTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "audit_total",
			Help: "Durable type result/decision recording outcomes (not application success).",
		}, []string{"outcome"}),
		invocationsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "invocations_total",
			Help: "Unknown, unattached results admitted to fallback gating. Legacy reason=unmatched includes successful unresolved workflows; it does not imply a returned error or HTTP call.",
		}, []string{"reason"}),
		gateRejectsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "gate_rejects_total",
			Help: "Invocations rejected by a gate before any LLM call. reason=privacy | plausibility | size | files | config.",
		}, []string{"reason"}),
		cacheHitsTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cache_hits_total",
			Help: "Decisions served from the LRU without hitting OpenAI.",
		}),
		cacheMissesTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cache_misses_total",
			Help: "Cache misses, including those later denied by admission; use calls_total for dispatches.",
		}),
		callDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "call_duration_seconds",
			Help:    "OpenAI round-trip duration.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
		}),
		callErrorsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "call_errors_total",
			Help: "OpenAI call failures by class: timeout | http_status | decode | schema.",
		}, []string{"class"}),
		decisionsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "decisions_total",
			Help: "LLM-produced decisions (shadow + live), labelled by media_type.",
		}, []string{"media_type"}),
		liveAppliedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "live_applied_total",
			Help: "LLM decisions that replaced inner result in live mode, labelled by media_type.",
		}, []string{"media_type"}),
		shadowSkippedTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "shadow_skipped_total",
			Help: "Shadow-mode invocations that observed a decision without applying it.",
		}),
	}
	// Zero-valued series distinguish an installed but unreachable stage from
	// missing instrumentation. Labels are a fixed finite vocabulary.
	m.invocationsTotal.WithLabelValues("unmatched")
	for _, reason := range []string{"runtime_flag", "privacy", "plausibility", "size", "files", "config", "request_size", "concurrency", "budget_cooldown", "budget_exhausted", "budget_unavailable", "audit_unavailable", "already_captured", "policy_live_unavailable"} {
		m.gateRejectsTotal.WithLabelValues(reason)
	}
	for _, outcome := range []string{"result_error", "result_recorded", "decision_error", "decision_recorded"} {
		m.auditTotal.WithLabelValues(outcome)
	}
	for _, category := range []string{"movie", "tv", "music", "audiobook", "book", "unknown"} {
		m.decisionsTotal.WithLabelValues(category)
		m.liveAppliedTotal.WithLabelValues(category)
	}
	return m
}

func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.invocationsTotal, m.gateRejectsTotal,
		m.cacheHitsTotal, m.cacheMissesTotal, m.callDuration,
		m.callErrorsTotal, m.decisionsTotal, m.liveAppliedTotal,
		m.shadowSkippedTotal, m.callsTotal, m.tokensTotal,
		m.usageMissingTotal, m.auditTotal, m.config,
	}
}
