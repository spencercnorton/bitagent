package liveness

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics exposes counters and gauges the ops dashboard reads to
// reason about liveness flow. All counters are monotonic; the
// blacklist size is a sampled gauge (see SampleBlacklistSize on the
// revalidator).
type Metrics struct {
	observations    *dualemit.CounterVec
	blacklistSize   *dualemit.Gauge
	revalidations   *dualemit.CounterVec
	torznabExcluded *dualemit.Counter
}

// NewMetrics constructs the metric set.
func NewMetrics() *Metrics {
	const (
		namespace = "bitagent"
		subsystem = "liveness"
	)
	return &Metrics{
		observations: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "observations_total",
			Help: "Liveness observations dispatched to the resolver, labelled by class " +
				"(alive/suspect) and outcome (upsert/error/skip_private/blacklisted/dht_recovered).",
		}, []string{"class", "outcome"}),
		blacklistSize: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "blacklist_size",
			Help: "Current count of torrent_liveness rows in status='dead'. Sampled by the revalidator worker.",
		}),
		revalidations: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "revalidations_total",
			Help: "Outcomes of DHT revalidation attempts on dead infohashes (alive_again/still_dead/error).",
		}, []string{"outcome"}),
		torznabExcluded: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "torznab_excluded_total",
			Help: "Search result items dropped by the Torznab adapter because their info_hash is currently dead.",
		}),
	}
}

// Collectors returns the metric set for registration under the
// prometheus_collectors fx group.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.observations,
		m.blacklistSize,
		m.revalidations,
		m.torznabExcluded,
	}
}

// Observation increments the observations counter.
func (m *Metrics) Observation(class, outcome string) {
	m.observations.WithLabelValues(class, outcome).Inc()
}

// SetBlacklistSize updates the blacklist gauge.
func (m *Metrics) SetBlacklistSize(n float64) {
	m.blacklistSize.Set(n)
}

// Revalidation records the outcome of a single revalidation
// attempt. Outcome must be one of: "alive_again", "still_dead",
// "error".
func (m *Metrics) Revalidation(outcome string) {
	m.revalidations.WithLabelValues(outcome).Inc()
}

// TorznabExcluded increments the count of search results dropped
// because the info_hash is dead.
func (m *Metrics) TorznabExcluded() {
	m.torznabExcluded.Inc()
}
