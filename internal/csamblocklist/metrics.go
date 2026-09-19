package csamblocklist

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the Prometheus surface for the CSAM blocklist. All
// counters and gauges share the bitagent_csam_blocklist_* prefix.
//
// Convention: feed labels are the URL host+path (no scheme, no query)
// to keep cardinality bounded. Reasons are stable strings.
type Metrics struct {
	// PrefetchBlocksTotal counts hashes rejected pre-fetch by this
	// blocklist. Mirrors blocking_manager.filter outcome but for
	// community-feed-known hashes specifically.
	PrefetchBlocksTotal prometheus.Counter

	// LookupsTotal counts every IsBlocked / Filter call (per hash).
	// Useful as a sanity gauge that the hot path is wired.
	LookupsTotal prometheus.Counter

	// FeedRefreshTotal labels by feed and outcome (success / error).
	FeedRefreshTotal *prometheus.CounterVec

	// FeedRefreshDurationSeconds labels by feed.
	FeedRefreshDurationSeconds *prometheus.HistogramVec

	// FeedEntriesGauge labels by feed; current entry count.
	FeedEntriesGauge *prometheus.GaugeVec

	// EntriesGauge is the union across all feeds.
	EntriesGauge prometheus.Gauge

	// ExportTotal labels by outcome (local_ok / local_err /
	// upstream_ok / upstream_err / skipped_no_match).
	ExportTotal *prometheus.CounterVec
}

// NewMetrics constructs the metric set. Call Collectors() to get the
// list to register with Prometheus.
func NewMetrics() *Metrics {
	const (
		ns  = "bitagent"
		sub = "csam_blocklist"
	)
	return &Metrics{
		PrefetchBlocksTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "prefetch_blocks_total",
			Help: "Infohashes rejected pre-fetch by the CSAM blocklist (community-feed-known double-hashes).",
		}),
		LookupsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "lookups_total",
			Help: "Total IsBlocked / Filter lookups (per hash).",
		}),
		FeedRefreshTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "feed_refresh_total",
			Help: "Per-feed refresh attempts. outcome=success|error.",
		}, []string{"feed", "outcome"}),
		FeedRefreshDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Subsystem: sub, Name: "feed_refresh_duration_seconds",
			Help:    "Per-feed refresh wallclock duration.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10), // 50ms .. ~25s
		}, []string{"feed"}),
		FeedEntriesGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: "feed_entries",
			Help: "Per-feed entry count from the most recent successful refresh.",
		}, []string{"feed"}),
		EntriesGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: "entries",
			Help: "Total double-hash entries in the active bloom filter.",
		}),
		ExportTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "export_total",
			Help: "Self-export outcomes. outcome=local_ok|local_err|upstream_ok|upstream_err|skipped_no_match.",
		}, []string{"outcome"}),
	}
}

// Collectors returns the slice for fx-group registration.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.PrefetchBlocksTotal,
		m.LookupsTotal,
		m.FeedRefreshTotal,
		m.FeedRefreshDurationSeconds,
		m.FeedEntriesGauge,
		m.EntriesGauge,
		m.ExportTotal,
	}
}
