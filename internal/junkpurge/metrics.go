package junkpurge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics for the junk-purge worker. Dry-run (would_delete) and live
// (deleted) counts share the metric surface so the trend can be compared
// directly before enable_purge is flipped.
type Metrics struct {
	cyclesTotal      *dualemit.Counter
	cycleDuration    *dualemit.Histogram
	candidatesTotal  *dualemit.Counter
	judgedTotal      *dualemit.CounterVec // by verdict
	wouldDeleteTotal *dualemit.Counter
	quarantinedTotal *dualemit.Counter
	expiredTotal     *dualemit.Counter
	llmDeferredTotal *dualemit.Counter
	circuitBreaks    *dualemit.CounterVec // by reason
	cycleErrorsTotal *dualemit.CounterVec // by stage
	cycleOutcomes    *dualemit.CounterVec // by terminal outcome
	cycleJudgments   *dualemit.CounterVec // recorded judgments by cycle outcome
	cycleJunk        *dualemit.CounterVec // confident junk by cycle outcome
	lastCycleUnix    *dualemit.Gauge

	// Provider accounting. "processing" is standard|grouped|batch and "type" is
	// input|cached_input|cache_write|output|reasoning. These are provider-
	// reported values, not estimates.
	llmRequests   *dualemit.CounterVec
	llmFailures   *dualemit.CounterVec
	llmTokens     *dualemit.CounterVec
	batchJobs     *dualemit.CounterVec
	batchItems    *dualemit.CounterVec
	batchInFlight *dualemit.Gauge
}

func NewMetrics() *Metrics {
	const (
		namespace = "bitagent"
		subsystem = "junkpurge"
	)
	return &Metrics{
		cyclesTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycles_total",
			Help: "Completed junk-purge cycles (dry-run or live).",
		}),
		cycleDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycle_duration_seconds",
			Help:    "Wall-clock duration of one junk-purge cycle.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		}),
		candidatesTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "candidates_total",
			Help: "Cumulative unmatched movie/tv torrents selected for judging (before per-cycle cap).",
		}),
		judgedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "judged_total",
			Help: "LLM verdicts by class: junk | real_mangled | real_absent | unsure.",
		}, []string{"verdict"}),
		wouldDeleteTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "would_delete_total",
			Help: "Cumulative confident-junk rows that WOULD be deleted if enable_purge were true (dry-run signal).",
		}),
		quarantinedTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "quarantined_total",
			Help: "Cumulative confident-junk torrents moved to junkpurge_quarantine (removed from the main DB; restorable during the review window).",
		}),
		expiredTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "expired_total",
			Help: "Cumulative quarantine entries permanently deleted + blacklisted after the review window.",
		}),
		llmDeferredTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "llm_deferred_total",
			Help: "Candidates left unjudged this cycle because the LLM was unavailable.",
		}),
		circuitBreaks: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "circuit_breaks_total",
			Help: "Cycles whose application was paused by a circuit breaker, by reason: capture_unavailable | llm_unavailable | junk_rate_anomaly.",
		}, []string{"reason"}),
		cycleErrorsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycle_errors_total",
			Help: "Cycle errors by stage: query | claim | capture | judge_group | judge | record | record_outcome | quarantine | expire.",
		}, []string{"stage"}),
		cycleOutcomes: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycle_outcomes_total",
			Help: "Junk-purge cycles by terminal outcome. Sum this metric, not inferred absence of another series, to reconcile cycles_total.",
		}, []string{"outcome"}),
		cycleJudgments: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycle_judgments_total",
			Help: "Valid judgments retained by a cycle or durable Batch run, attributed to its terminal outcome. Breaker-blocked judgments remain evaluation evidence.",
		}, []string{"outcome"}),
		cycleJunk: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycle_confident_junk_total",
			Help: "Recorded confident-junk judgments attributed to the cycle's terminal outcome; breaker outcomes were not actioned.",
		}, []string{"outcome"}),
		lastCycleUnix: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "last_cycle_unix_seconds",
			Help: "Unix time of the most recent completed junk-purge cycle.",
		}),
		llmRequests: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "llm_requests_total",
			Help: "Provider requests by processing mode, model and outcome.",
		}, []string{"processing", "model", "outcome"}),
		llmFailures: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "llm_failures_total",
			Help: "Failed provider requests by processing mode, model and bounded reason: canceled | timeout | transport | response_read | rate_limited | server_error | request_rejected | request_build | budget_exhausted | budget_unavailable | unavailable | invalid_response.",
		}, []string{"processing", "model", "reason"}),
		llmTokens: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "llm_tokens_total",
			Help: "Provider-reported tokens by processing mode, model and token type. cached_input is a subset of input and reasoning is a subset of output; do not sum types.",
		}, []string{"processing", "model", "type"}),
		batchJobs: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "batch_jobs_total",
			Help: "Provider Batch jobs by lifecycle outcome.",
		}, []string{"outcome"}),
		batchItems: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "batch_items_total",
			Help: "Provider Batch items by settlement outcome.",
		}, []string{"outcome"}),
		batchInFlight: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "batch_in_flight",
			Help: "Durable provider Batch manifests not yet processed.",
		}),
	}
}

func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.cyclesTotal, m.cycleDuration, m.candidatesTotal, m.judgedTotal,
		m.wouldDeleteTotal, m.quarantinedTotal, m.expiredTotal, m.llmDeferredTotal,
		m.circuitBreaks, m.cycleErrorsTotal, m.cycleOutcomes,
		m.cycleJudgments, m.cycleJunk, m.lastCycleUnix,
		m.llmRequests, m.llmFailures, m.llmTokens,
		m.batchJobs, m.batchItems, m.batchInFlight,
	}
}

func (m *Metrics) observeCycleOutcome(outcome string, judged, confidentJunk int) {
	m.cycleOutcomes.WithLabelValues(outcome).Inc()
	if judged > 0 {
		m.cycleJudgments.WithLabelValues(outcome).Add(float64(judged))
	}
	if confidentJunk > 0 {
		m.cycleJunk.WithLabelValues(outcome).Add(float64(confidentJunk))
	}
}

type tokenUsage struct {
	Input      int64
	Cached     int64
	CacheWrite int64
	Output     int64
	Reasoning  int64
}

func (m *Metrics) observeProviderUsage(processing, model string, usage tokenUsage) {
	values := []struct {
		kind  string
		value int64
	}{
		{"input", usage.Input},
		{"cached_input", usage.Cached},
		{"cache_write", usage.CacheWrite},
		{"output", usage.Output},
		{"reasoning", usage.Reasoning},
	}
	for _, value := range values {
		if value.value > 0 {
			m.llmTokens.WithLabelValues(processing, model, value.kind).Add(float64(value.value))
		}
	}
}
