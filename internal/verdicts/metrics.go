package verdicts

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics observes the phase-B ledger readers. One counter, labelled:
//
//	reader:  torznab | crawler
//	outcome: torznab — ledger_only (ledger would exclude, liveness would
//	         not: the resurrection cohort the ledger exists to kill),
//	         liveness_only (dead but not in the ledger — expected to be
//	         large until phase C backfills all mechanisms), both,
//	         excluded (live-mode ledger-driven drop), error;
//	         crawler — would_skip (shadow), skipped (live), error.
//
// The shadow-review gate reads ledger_only and would_skip: they are the
// behavior change a flag flip would introduce.
type Metrics struct {
	reader *dualemit.CounterVec
}

func NewMetrics() *Metrics {
	return &Metrics{
		reader: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "bitagent", Subsystem: "verdicts", Name: "reader_total",
			Help: "Phase-B verdict-ledger reader observations by reader and outcome " +
				"(shadow divergences while VERDICTS_READERS_ENABLED=false; exclusions when live).",
		}, []string{"reader", "outcome"}),
	}
}

func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.reader}
}

// Reader adds n to the (reader, outcome) counter. Nil-safe so callers can
// hold an optional *Metrics without guarding every increment.
func (m *Metrics) Reader(reader, outcome string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.reader.WithLabelValues(reader, outcome).Add(float64(n))
}
