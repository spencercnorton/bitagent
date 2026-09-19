package evidence

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics exposes counters the ops dashboard reads to reason about
// evidence flow and canonical-label coverage. All counters are
// monotonic; gauges are updated on each resolve.
type Metrics struct {
	eventsReceived    *dualemit.CounterVec
	eventsPersisted   *dualemit.CounterVec
	eventsDuplicated  *dualemit.CounterVec
	eventsRejected    *dualemit.CounterVec
	canonicalUpserts  *dualemit.CounterVec
	canonicalNoChange *dualemit.CounterVec
	sourceErrorsTotal *dualemit.CounterVec
	sourcePollSeconds *dualemit.HistogramVec
}

// Collectors returns the metric set for registration under the
// prometheus_collectors fx group.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.eventsReceived, m.eventsPersisted, m.eventsDuplicated,
		m.eventsRejected, m.canonicalUpserts, m.canonicalNoChange,
		m.sourceErrorsTotal, m.sourcePollSeconds,
	}
}

// NewMetrics constructs the metric set. It is safe to call once per
// process; counters are internally registered into the returned struct
// but only become visible once Collectors() is registered on the
// shared Prometheus registry.
func NewMetrics() *Metrics {
	const (
		namespace = "bitagent"
		subsystem = "evidence"
	)
	labels := []string{"source", "kind"}
	return &Metrics{
		eventsReceived: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "events_received_total",
			Help: "Evidence events observed by the ingestor before any filtering.",
		}, labels),
		eventsPersisted: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "events_persisted_total",
			Help: "Evidence events that resulted in a new label_evidence row.",
		}, labels),
		eventsDuplicated: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "events_duplicated_total",
			Help: "Evidence events rejected by the dedupe unique index (expected on webhook retries and poll overlaps).",
		}, labels),
		eventsRejected: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "events_rejected_total",
			Help: "Evidence events that could not be persisted (no join key, malformed payload, auth failure).",
		}, []string{"source", "kind", "reason"}),
		canonicalUpserts: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "canonical_upserts_total",
			Help: "New winning labels applied to torrent_canonical_labels (insert or strength-beating update).",
		}, []string{"source", "media_type"}),
		canonicalNoChange: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "canonical_no_change_total",
			Help: "Evidence writes that did not move torrent_canonical_labels because an equal-or-stronger row already won.",
		}, []string{"source"}),
		sourceErrorsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "source_errors_total",
			Help: "Source-side errors during poll or webhook processing (network, auth, parse).",
		}, []string{"source", "instance", "stage"}),
		sourcePollSeconds: dualemit.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "source_poll_duration_seconds",
			Help:    "Wall-clock duration of a single source poll cycle.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // 0.1s … ~51s
		}, []string{"source", "instance"}),
	}
}

// Received increments the received counter.
func (m *Metrics) Received(src Source, kind Kind) {
	m.eventsReceived.WithLabelValues(string(src), string(kind)).Inc()
}

// Persisted increments the persisted counter.
func (m *Metrics) Persisted(src Source, kind Kind) {
	m.eventsPersisted.WithLabelValues(string(src), string(kind)).Inc()
}

// Duplicated increments the dedupe counter.
func (m *Metrics) Duplicated(src Source, kind Kind) {
	m.eventsDuplicated.WithLabelValues(string(src), string(kind)).Inc()
}

// Rejected increments the rejection counter with a reason label.
// Reason should be one of a small stable set: "no_join_key",
// "malformed", "auth", "timeout", "store_error".
func (m *Metrics) Rejected(src Source, kind Kind, reason string) {
	m.eventsRejected.WithLabelValues(string(src), string(kind), reason).Inc()
}

// CanonicalUpsert increments the canonical-label win counter.
func (m *Metrics) CanonicalUpsert(src Source, mt MediaType) {
	if mt == "" {
		mt = MediaTypeUnknown
	}
	m.canonicalUpserts.WithLabelValues(string(src), string(mt)).Inc()
}

// CanonicalNoChange increments the canonical-label no-change counter.
func (m *Metrics) CanonicalNoChange(src Source) {
	m.canonicalNoChange.WithLabelValues(string(src)).Inc()
}

// SourceError increments the source-error counter.
// Stage should be one of a small stable set: "auth", "fetch",
// "parse", "store".
func (m *Metrics) SourceError(src Source, instance, stage string) {
	m.sourceErrorsTotal.WithLabelValues(string(src), instance, stage).Inc()
}

// ObservePoll records a poll cycle's duration.
func (m *Metrics) ObservePoll(src Source, instance string, seconds float64) {
	m.sourcePollSeconds.WithLabelValues(string(src), instance).Observe(seconds)
}
