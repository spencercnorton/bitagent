package importer

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics observes the /import ingest gate (T3 phase C, design §2.3 — closing
// the mechanism 6+7 bloom bypass). One counter, labelled by the gate that
// dropped the hash:
//
//	csam     — community-known CSAM feed bloom (always enforced)
//	blocking — self-observed infohash blocklist bloom (always enforced)
//
// A non-zero csam/blocking count is the proof the bypass is closed: hashes an
// operator/feed tried to import that the crawler would already refuse.
type Metrics struct {
	gated *dualemit.CounterVec
}

func NewMetrics() *Metrics {
	return &Metrics{
		gated: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "bitagent", Subsystem: "import", Name: "gated_total",
			Help: "Infohashes dropped from an /import batch by the CSAM/blocking gate " +
				"(closes the mechanism 6+7 bypass — /import previously skipped both blooms).",
		}, []string{"gate"}),
	}
}

func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.gated}
}

// Gated adds n drops attributed to a gate. Nil-safe so a narrow test can
// construct an importer without wiring metrics.
func (m *Metrics) Gated(gate string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.gated.WithLabelValues(gate).Add(float64(n))
}
