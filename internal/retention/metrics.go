package retention

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics for the retention worker. All counters are labelled by
// `action` where applicable so dry-run counts and real purges share
// a single metric family and can be compared directly.
type Metrics struct {
	cyclesTotal      *dualemit.Counter
	cycleDuration    *dualemit.Histogram
	candidatesTotal  *dualemit.Counter
	wouldPurgeTotal  *dualemit.Counter
	purgedTotal      *dualemit.Counter
	cycleErrorsTotal *dualemit.CounterVec
	lastCycleUnix    *dualemit.Gauge
}

func NewMetrics() *Metrics {
	const (
		namespace = "bitagent"
		subsystem = "retention"
	)
	return &Metrics{
		cyclesTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycles_total",
			Help: "Completed retention cycles (dry-run or live).",
		}),
		cycleDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycle_duration_seconds",
			Help:    "Wall-clock duration of one retention cycle.",
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 10),
		}),
		candidatesTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "candidates_total",
			Help: "Cumulative rows matched by the purge predicate before any cap was applied.",
		}),
		wouldPurgeTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "would_purge_total",
			Help: "Cumulative rows that WOULD be purged if enable_purge were true. Metric for dry-run decision-making.",
		}),
		purgedTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "purged_total",
			Help: "Cumulative rows actually deleted from torrents (cascades through FKs).",
		}),
		cycleErrorsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "cycle_errors_total",
			Help: "Cycle errors by stage: query | delete | commit.",
		}, []string{"stage"}),
		lastCycleUnix: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: subsystem, Name: "last_cycle_unix_seconds",
			Help: "Unix time of the most recent completed retention cycle.",
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
