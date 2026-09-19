package priors

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics for the priors module. Mirrors the liveness package shape:
// counters are monotonic, gauges are sampled by the worker, and the
// reranker exposes a histogram of how far re-ordering moved items so
// operators can see the ranking effect at a glance.
type Metrics struct {
	observations    *dualemit.CounterVec
	pendingGauge    *dualemit.Gauge
	expiredTotal    *dualemit.Counter
	rerankBatch     *dualemit.Counter
	rerankItems     *dualemit.Counter
	rerankShift     *dualemit.Histogram
	keyValueCount   *dualemit.GaugeVec
	totalObsByClass *dualemit.CounterVec
}

func NewMetrics() *Metrics {
	const (
		namespace = "bitagent"
		subsystem = "priors"
	)
	return &Metrics{
		observations: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "observations_total",
			Help: "Priors module observations. event ∈ {grab_recorded, import_resolved, expirer_resolved, lookup_error}, " +
				"outcome ∈ {ok, error}.",
		}, []string{"event", "outcome"}),
		pendingGauge: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "pending_grabs",
			Help: "Current count of torrent_grab_attempts rows with resolved_at IS NULL. Sampled by the expirer worker.",
		}),
		expiredTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "expired_total",
			Help: "Pending grab attempts that crossed ResolutionWindow without a matching import and were marked failure.",
		}),
		rerankBatch: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "rerank_batches_total",
			Help: "Torznab search responses the priors ranker SCORED. Incremented in both modes — " +
				"shadow (Apply=false) included, since ranker.go records displacement above the " +
				"Apply check so shadow can produce the evidence that justifies flipping it. " +
				"A zero here means the ranker never ran, NOT that it ran without reordering.",
		}),
		rerankItems: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "rerank_items_total",
			Help: "Total result items inspected by the priors ranker across all batches.",
		}),
		rerankShift: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "rerank_shift_positions",
			Help: "Per-item rank-position delta caused by the ranker (signed: positive = moved down, negative = moved up). " +
				"Concentration near zero means the priors are not changing the result order much.",
			Buckets: []float64{-32, -16, -8, -4, -2, -1, 0, 1, 2, 4, 8, 16, 32},
		}),
		keyValueCount: dualemit.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "key_values",
			Help: "Distinct key values currently stored, broken down by key_type. Sampled by the expirer worker.",
		}, []string{"key_type"}),
		totalObsByClass: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "outcomes_total",
			Help: "Resolved grab outcomes by class (success/failure). Mirrors the underlying torrent_grab_attempts " +
				"resolutions; useful for live success-rate sparklines independent of the priors table.",
		}, []string{"class"}),
	}
}

func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.observations, m.pendingGauge, m.expiredTotal, m.rerankBatch,
		m.rerankItems, m.rerankShift, m.keyValueCount, m.totalObsByClass,
	}
}

// Observation increments the observations counter.
func (m *Metrics) Observation(event, outcome string) {
	m.observations.WithLabelValues(event, outcome).Inc()
}

// SetPending updates the pending-grabs gauge.
func (m *Metrics) SetPending(n float64) {
	m.pendingGauge.Set(n)
}

// Expired records that n pending grab attempts were marked failure
// by the expirer.
func (m *Metrics) Expired(n int) {
	if n <= 0 {
		return
	}
	m.expiredTotal.Add(float64(n))
}

// RerankBatch records one search response that was ranked.
func (m *Metrics) RerankBatch(items int) {
	m.rerankBatch.Inc()
	if items > 0 {
		m.rerankItems.Add(float64(items))
	}
}

// RerankShift records the rank-position delta for one item.
func (m *Metrics) RerankShift(delta int) {
	m.rerankShift.Observe(float64(delta))
}

// SetKeyValueCount publishes the distinct-key count for a key_type.
func (m *Metrics) SetKeyValueCount(keyType string, n float64) {
	m.keyValueCount.WithLabelValues(keyType).Set(n)
}

// Outcome records a resolved attempt by class.
func (m *Metrics) Outcome(class string) {
	m.totalObsByClass.WithLabelValues(class).Inc()
}
