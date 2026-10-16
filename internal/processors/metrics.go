package processors

import (
	"github.com/prometheus/client_golang/prometheus"
	"sync"
)

type Metrics struct {
	attempts  *prometheus.CounterVec
	outcomes  *prometheus.CounterVec
	wait      *prometheus.HistogramVec
	duration  *prometheus.HistogramVec
	admission *prometheus.CounterVec
	jobs      *prometheus.GaugeVec
	registry  *prometheus.GaugeVec
	mu        sync.Mutex
	seen      map[string][]string
}

func NewMetrics() *Metrics {
	return &Metrics{
		prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_processors_attempts_total", Help: "Process-lifetime dispatched optional attempts"}, []string{"processor_id", "generation"}),
		prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_processors_outcomes_total", Help: "Process-lifetime optional attempt outcomes"}, []string{"processor_id", "generation", "stage", "outcome"}),
		prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "bitagent_processors_queue_wait_seconds", Help: "Queue waiting before each dispatched attempt", Buckets: prometheus.ExponentialBuckets(0.1, 4, 9)}, []string{"processor_id", "generation", "stage"}),
		prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "bitagent_processors_request_duration_seconds", Help: "Core-measured service request duration", Buckets: prometheus.DefBuckets}, []string{"processor_id", "generation", "stage"}),
		prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_processors_admission_total", Help: "Optional admission decisions"}, []string{"outcome"}),
		prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bitagent_processors_jobs", Help: "Retained durable optional jobs"}, []string{"state"}),
		prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bitagent_processors_registry_info", Help: "Active registry execution versions"}, []string{"processor_id", "generation", "version", "model_version", "policy_version"}),
		sync.Mutex{}, map[string][]string{},
	}
}
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.attempts, m.outcomes, m.wait, m.duration, m.admission, m.jobs, m.registry}
}

func (m *Metrics) note(id, generation string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen[id+"/"+generation] = []string{id, generation}
}
func (m *Metrics) prune(active map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, labels := range m.seen {
		if !active[key] {
			match := prometheus.Labels{"processor_id": labels[0], "generation": labels[1]}
			m.attempts.DeletePartialMatch(match)
			m.outcomes.DeletePartialMatch(match)
			m.wait.DeletePartialMatch(match)
			m.duration.DeletePartialMatch(match)
			delete(m.seen, key)
		}
	}
}
