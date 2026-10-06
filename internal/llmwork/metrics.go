package llmwork

import "github.com/prometheus/client_golang/prometheus"

// Metrics uses only fixed lifecycle labels, never source names, hashes or
// provider bodies. Queue denials remain observable even when ingestion keeps
// its ordinary deterministic result.
type Metrics struct {
	outcomes *prometheus.CounterVec
	cycles   *prometheus.CounterVec
	depth    *prometheus.GaugeVec
}

func NewMetrics() *Metrics {
	return &Metrics{
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_llm_work_admission_total", Help: "Optional model queue admission outcomes."}, []string{"kind", "outcome"}),
		cycles:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_llm_work_cycles_total", Help: "Owned optional model worker cycles."}, []string{"outcome"}),
		depth:    prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bitagent_llm_work_tasks", Help: "Optional model tasks by stage and lifecycle state."}, []string{"kind", "state"}),
	}
}
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.outcomes, m.cycles, m.depth}
}

func (s *Store) Metrics() *Metrics { return s.metrics }
