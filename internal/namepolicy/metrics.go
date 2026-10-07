package namepolicy

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

func newDeniedMetric() *dualemit.CounterVec {
	return dualemit.NewCounterVec(prometheus.CounterOpts{Namespace: "bitagent", Subsystem: "name_policy", Name: "denied_total", Help: "Denied work or eligibility checks by bounded stage/reason; checks are not unique releases or inferred saved dollars."}, []string{"stage", "reason"})
}
func (p *Policy) Collectors() []prometheus.Collector { return []prometheus.Collector{p.denied} }
func (p *Policy) Observe(stage string, d Decision) {
	if p == nil || p.denied == nil || d.Eligible {
		return
	}
	switch stage {
	case "crawler_acquisition", "crawler_existing", "import", "processor", "classifier", "model", "model_dispatch", "task_enqueue", "model_apply", "private_name_check", "public_hash_check", "public_grab":
	default:
		stage = "other"
	}
	switch d.Reason {
	case ReasonOwnerHash, ReasonMissingName, ReasonHan, ReasonCyrillic, ReasonAdultComposite, ReasonAdultType, ReasonNotServed:
	default:
		return
	}
	p.denied.WithLabelValues(stage, d.Reason).Inc()
}
