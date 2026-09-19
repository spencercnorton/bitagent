package wantbridge

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics is the wantbridge telemetry surface. All counters and
// histograms are dual-emitted (bitmagnet_* legacy + bitagent_*
// canonical) so existing dashboards keep working.
//
// Surface:
//
//	matches_total{tier,source}                — every Match() outcome
//	priority_fetches_total{tier}              — fetcher-side enqueues (D4 follow-up)
//	skipped_total{reason}                     — Tier 2 pre-fetch drops (D4 follow-up)
//	wantlist_size{source}                     — gauge per *arr
//	arr_poll_errors_total{source}             — failed *arr polls
//	arr_poll_duration_seconds{source}         — histogram of poll latencies
//	fingerprint_rebuild_duration_seconds      — histogram of table-rebuild latencies
//	fingerprint_keys                          — gauge of canonical-key count
type Metrics struct {
	matches               *dualemit.CounterVec // labels: tier, source
	priorityFetches       *dualemit.CounterVec // labels: tier   (D4 follow-up wires this)
	skipped               *dualemit.CounterVec // labels: reason (D4 follow-up wires this)
	wantlistSize          *dualemit.GaugeVec   // labels: source
	arrPollErrors         *dualemit.CounterVec // labels: source
	arrPollDuration       *dualemit.HistogramVec
	fingerprintRebuildDur *dualemit.Histogram
	fingerprintKeys       *dualemit.Gauge
}

const (
	wbNamespace = "bitagent"
	wbSubsystem = "wantbridge"
)

// NewMetrics builds the metric set. Call Collectors() for the
// prometheus_collectors fx group.
func NewMetrics() *Metrics {
	return &Metrics{
		matches: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: wbNamespace,
			Subsystem: wbSubsystem,
			Name:      "matches_total",
			Help:      "Wantbridge Match() outcomes by tier and source. tier=tier0|tier1|tier2; source=sonarr|radarr|lidarr|empty (Tier 1 / Tier 2 have no source).",
		}, []string{"tier", "source"}),
		priorityFetches: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: wbNamespace,
			Subsystem: wbSubsystem,
			Name:      "priority_fetches_total",
			Help:      "BEP-9 fetches initiated from each wantbridge tier. Wired by the dhtcrawler queue-priority shim (D4 follow-up MR).",
		}, []string{"tier"}),
		skipped: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: wbNamespace,
			Subsystem: wbSubsystem,
			Name:      "skipped_total",
			Help:      "Discoveries dropped pre-fetch (Tier 2). reason=non_latin_script|blocked_extension|nsfw_keyword. Wired by the dhtcrawler queue-priority shim (D4 follow-up MR).",
		}, []string{"reason"}),
		wantlistSize: dualemit.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: wbNamespace,
			Subsystem: wbSubsystem,
			Name:      "wantlist_size",
			Help:      "Current wantlist entries per *arr source (post-canonicalisation, one entry per indexable canonical key).",
		}, []string{"source"}),
		arrPollErrors: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: wbNamespace,
			Subsystem: wbSubsystem,
			Name:      "arr_poll_errors_total",
			Help:      "Failed *arr poll attempts by source. Stale-but-present fingerprints are kept until ConsecutiveErr crosses 5; after that the source's entries are evicted.",
		}, []string{"source"}),
		arrPollDuration: dualemit.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: wbNamespace,
			Subsystem: wbSubsystem,
			Name:      "arr_poll_duration_seconds",
			Help:      "Wall-clock latency of *arr poll calls.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0},
		}, []string{"source"}),
		fingerprintRebuildDur: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: wbNamespace,
			Subsystem: wbSubsystem,
			Name:      "fingerprint_rebuild_duration_seconds",
			Help:      "Wall-clock latency of full fingerprint-table rebuilds (one per refresh cycle).",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
		}),
		fingerprintKeys: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: wbNamespace,
			Subsystem: wbSubsystem,
			Name:      "fingerprint_keys",
			Help:      "Total canonical keys currently indexed across all *arr sources.",
		}),
	}
}

// Collectors returns every collector for fx-group registration.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.matches, m.priorityFetches, m.skipped,
		m.wantlistSize, m.arrPollErrors, m.arrPollDuration,
		m.fingerprintRebuildDur, m.fingerprintKeys,
	}
}

// Callbacks builds a ServiceCallbacks struct wired to this Metrics
// instance. Pass to NewWith / NewService. Hooks are called from the
// hot path so they MUST be lock-free; dualemit's atomic counters
// satisfy that.
func (m *Metrics) Callbacks() ServiceCallbacks {
	return ServiceCallbacks{
		OnMatch: func(tier Tier, source Source) {
			m.matches.With(prometheus.Labels{
				"tier":   tier.String(),
				"source": string(source),
			}).Inc()
		},
		OnPollOK: func(source Source, n int, d time.Duration) {
			m.wantlistSize.With(prometheus.Labels{"source": string(source)}).Set(float64(n))
			m.arrPollDuration.With(prometheus.Labels{"source": string(source)}).Observe(d.Seconds())
		},
		OnPollErr: func(source Source, _ error) {
			m.arrPollErrors.With(prometheus.Labels{"source": string(source)}).Inc()
		},
		OnFingerprintRebuild: func(total int, d time.Duration) {
			m.fingerprintKeys.Set(float64(total))
			m.fingerprintRebuildDur.Observe(d.Seconds())
		},
	}
}

// PriorityFetchObserver returns a closure the dhtcrawler can invoke
// from its queue-priority shim (D4 follow-up MR) to record a tier
// decision at fetch-time.
func (m *Metrics) PriorityFetchObserver() func(Tier) {
	return func(t Tier) {
		m.priorityFetches.With(prometheus.Labels{"tier": t.String()}).Inc()
	}
}

// SkippedObserver returns a closure for the Tier 2 pre-fetch skip
// path. Same wiring story as PriorityFetchObserver.
func (m *Metrics) SkippedObserver() func(reason string) {
	return func(reason string) {
		m.skipped.With(prometheus.Labels{"reason": reason}).Inc()
	}
}
