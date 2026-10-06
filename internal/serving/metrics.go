package serving

import "github.com/prometheus/client_golang/prometheus"

// Metrics exposes resolved visibility settings without row identity or content.
type Metrics struct{ config *prometheus.GaugeVec }

func NewMetrics(cfg Config) *Metrics {
	m := &Metrics{config: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "bitagent_serving_config", Help: "Resolved consumer visibility policy; not proof of classification accuracy.",
	}, []string{"setting"})}
	m.config.WithLabelValues("quarantine_exclusion").Set(1)
	m.config.WithLabelValues("quarantine_snapshot_binding_required").Set(1)
	var adult float64
	if cfg.ExcludeAdult {
		adult = 1
	}
	m.config.WithLabelValues("exclude_adult").Set(adult)
	return m
}

func (m *Metrics) Collectors() []prometheus.Collector { return []prometheus.Collector{m.config} }
