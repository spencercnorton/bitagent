package queueclean

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics for the queueclean worker. Mirrors retention.Metrics so the
// operator dashboards have a single mental model.
type Metrics struct {
	cyclesTotal      *dualemit.Counter
	cycleDuration    *dualemit.Histogram
	candidatesTotal  *dualemit.CounterVec // by status
	wouldPurgeTotal  *dualemit.CounterVec // by status
	purgedTotal      *dualemit.CounterVec // by status
	cycleErrorsTotal *dualemit.CounterVec // by stage
	lastCycleUnix    *dualemit.Gauge
}

func NewMetrics() *Metrics {
	const (
		namespace = "bitagent"
		subsystem = "queueclean"
	)
	return &Metrics{
		cyclesTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycles_total",
			Help: "Completed queueclean cycles (dry-run or live).",
		}),
		cycleDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycle_duration_seconds",
			Help:    "Wall-clock duration of one queueclean cycle.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
		}),
		candidatesTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "candidates_total",
			Help: "Cumulative rows matched by the purge predicate, per status.",
		}, []string{"status"}),
		wouldPurgeTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "would_purge_total",
			Help: "Cumulative rows that WOULD be purged if enable_purge=true. Per status.",
		}, []string{"status"}),
		purgedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "purged_total",
			Help: "Cumulative queue_jobs rows deleted. Per status.",
		}, []string{"status"}),
		cycleErrorsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycle_errors_total",
			Help: "Cycle errors by stage: query | delete.",
		}, []string{"stage"}),
		lastCycleUnix: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "last_cycle_unix_seconds",
			Help: "Unix time of the most recent completed queueclean cycle.",
		}),
	}
}

func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.cyclesTotal, m.cycleDuration, m.candidatesTotal,
		m.wouldPurgeTotal, m.purgedTotal, m.cycleErrorsTotal,
		m.lastCycleUnix,
	}
}
