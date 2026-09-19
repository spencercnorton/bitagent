package seeds

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics holds the seeds-worker counters and gauges. Counters describe cycle
// activity; gauges describe standing coverage (set at the end of each cycle)
// so the dashboard can show how much of the catalog trackers actually know
// about — the honest-unknown surface.
type Metrics struct {
	cyclesTotal   *dualemit.Counter
	cycleDuration *dualemit.Histogram
	cycleErrors   *dualemit.CounterVec // stage: select | scrape | persist

	hashesSelected  *dualemit.Counter
	positiveTotal   *dualemit.Counter // scrape found seeders/leechers > 0
	knownZeroTotal  *dualemit.Counter // a tracker has the hash but swarm is dead
	unknownTotal    *dualemit.Counter // no tracker in the pool knows the hash
	sourcesUpserted *dualemit.Counter // authoritative 'tracker' source rows written
	sourcesCleared  *dualemit.Counter // stale 'tracker' source rows removed
	denormSynced    *dualemit.Counter // torrent_contents rows refreshed from sources
	livenessRevived *dualemit.Counter // suspect/dead liveness rows revived by positive scrapes
	livenessSuspect *dualemit.Counter // suspect observations recorded from authoritative zeros

	trackerScrapes *dualemit.CounterVec // tracker, result: ok | error

	// Standing coverage gauges (set each cycle from a count query).
	ledgerRows   *dualemit.Gauge
	trackerKnown *dualemit.Gauge
	positiveRows *dualemit.Gauge
}

func NewMetrics() *Metrics {
	const ns, sub = "bitagent", "seeds"
	c := func(name, help string) *dualemit.Counter {
		return dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: name, Help: help,
		})
	}
	cv := func(name, help string, labels []string) *dualemit.CounterVec {
		return dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: name, Help: help,
		}, labels)
	}
	g := func(name, help string) *dualemit.Gauge {
		return dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: name, Help: help,
		})
	}
	return &Metrics{
		cyclesTotal: c("cycles_total", "Completed seeds refresh cycles (dry-run or live)."),
		cycleDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: ns, Subsystem: sub, Name: "cycle_duration_seconds",
			Help:    "Wall-clock duration of one seeds refresh cycle.",
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 12),
		}),
		cycleErrors:     cv("cycle_errors_total", "Cycle errors by stage: select | scrape | persist | denorm.", []string{"stage"}),
		hashesSelected:  c("hashes_selected_total", "Info-hashes selected for scraping across all cycles."),
		positiveTotal:   c("positive_total", "Scrapes that found a live swarm (seeders or leechers > 0)."),
		knownZeroTotal:  c("known_zero_total", "Scrapes where a tracker knew the hash but the swarm was dead."),
		unknownTotal:    c("unknown_total", "Scrapes where no tracker in the pool knew the hash."),
		sourcesUpserted: c("sources_upserted_total", "Authoritative 'tracker' source rows written."),
		sourcesCleared:  c("sources_cleared_total", "Stale 'tracker' source rows removed after a non-positive re-scrape."),
		denormSynced:    c("denorm_synced_total", "torrent_contents rows whose denormalized seeders/leechers were refreshed."),
		livenessRevived: c("liveness_revived_total", "Suspect/dead liveness rows flipped alive by a positive tracker scrape."),
		livenessSuspect: c("liveness_suspect_total", "Suspect observations recorded from authoritative tracker zeros."),
		trackerScrapes:  cv("tracker_scrapes_total", "Per-tracker scrape packets by result.", []string{"tracker", "result"}),
		ledgerRows:      g("ledger_rows", "Total rows in the tracker-seeds ledger (hashes checked at least once)."),
		trackerKnown:    g("tracker_known_rows", "Ledger rows a tracker knows about (positive or known-zero)."),
		positiveRows:    g("positive_rows", "Ledger rows currently carrying a positive (live) tracker count."),
	}
}

func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.cyclesTotal, m.cycleDuration, m.cycleErrors,
		m.hashesSelected, m.positiveTotal, m.knownZeroTotal, m.unknownTotal,
		m.sourcesUpserted, m.sourcesCleared, m.denormSynced, m.trackerScrapes,
		m.livenessRevived, m.livenessSuspect,
		m.ledgerRows, m.trackerKnown, m.positiveRows,
	}
}
