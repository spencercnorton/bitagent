package contentfilter

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Metrics retains the deterministic filter counters and their legacy aliases.
type Metrics struct {
	examined  *dualemit.Counter
	kept      *dualemit.Counter
	dropped   *dualemit.CounterVec
	wouldDrop *dualemit.CounterVec

	// blocked_ext_total{ext="..."} — sub-breakdown of drop_total where
	// reason="blocked_extension". Additive to drop_total; lets the
	// operator see which extensions actually fire so they can tune
	// the BlockedExtensions list. Only emitted when reason matches.
	blockedExt *dualemit.CounterVec
}

const (
	cfNamespace = "bitagent"
	cfSubsystem = "contentfilter"
	cfLabel     = "reason"
)

// NewMetrics builds the metric set. Call Collectors() to register
// with the prometheus_collectors fx group.
func NewMetrics() *Metrics {
	return &Metrics{
		examined: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "examined_total",
			Help:      "Torrents the content filter examined (post-classify, pre-persist).",
		}),
		kept: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "keep_total",
			Help:      "Torrents the content filter passed without dropping.",
		}),
		dropped: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "drop_total",
			Help:      "Torrents dropped by the content filter (the matching tier is enforced). Per-reason.",
		}, []string{cfLabel}),
		wouldDrop: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "would_drop_total",
			Help:      "Counterfactual drops while the matching tier is shadow. Per-reason; requires Enabled=true.",
		}, []string{cfLabel}),

		blockedExt: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: cfNamespace,
			Subsystem: cfSubsystem,
			Name:      "blocked_ext_total",
			Help:      "Torrents dropped because their primary extension is on the blocklist. Sub-breakdown of drop_total{reason=blocked_extension}. Use to tune BlockedExtensions.",
		}, []string{"ext"}),
	}
}

// Collectors returns every collector for fx-group registration.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.examined, m.kept, m.dropped, m.wouldDrop, m.blockedExt}
}

// Observe records the outcome of a Decide() call. Caller should
// invoke this exactly once per torrent decision.
func (m *Metrics) Observe(d Decision) {
	m.examined.Inc()
	switch {
	case d.WouldDrop && !d.Allow:
		// Live drop: increment the dropped counter for the reason.
		m.dropped.With(prometheus.Labels{cfLabel: d.Reason.String()}).Inc()
		if d.Reason == ReasonBlockedExtension && d.BlockedExt != "" {
			m.blockedExt.With(prometheus.Labels{"ext": d.BlockedExt}).Inc()
		}
	case d.WouldDrop && d.Allow:
		// Shadow mode: counterfactual visible only.
		m.wouldDrop.With(prometheus.Labels{cfLabel: d.Reason.String()}).Inc()
		if d.Reason == ReasonBlockedExtension && d.BlockedExt != "" {
			m.blockedExt.With(prometheus.Labels{"ext": d.BlockedExt}).Inc()
		}
		m.kept.Inc() // it was kept (shadow mode keeps everything)
	default:
		m.kept.Inc()
	}
}
