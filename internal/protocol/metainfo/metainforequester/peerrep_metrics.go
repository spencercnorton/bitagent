package metainforequester

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/peerrep"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// peerRepMetrics covers the three observability surfaces of the
// peer-reputation cache:
//
//	skippedTotal{class}     — Decide() said "skip" AND Enforce=true,
//	                          so the inner BEP-9 fetch was bypassed.
//	wouldSkipTotal{class}   — Decide() said "would skip" but
//	                          Enforce=false (shadow mode); the inner
//	                          fetch ran anyway. The counterfactual.
//	observedTotal{class}    — outcomes that fed the cache after a
//	                          fetch attempt (success or any of the
//	                          peerrep.ErrorClass values).
//
// All three carry a `class` label whose values are stable per
// `peerrep.ErrorClass.String()`. Labels are pinned, not free-form,
// so a future class addition needs the String() method updated to
// match.
type peerRepMetrics struct {
	skippedTotal   *dualemit.CounterVec
	wouldSkipTotal *dualemit.CounterVec
	observedTotal  *dualemit.CounterVec
}

const (
	peerRepNamespace = "bitagent"
	peerRepSubsystem = "peer_rep"
	peerRepLabel     = "class"
)

func newPeerRepMetrics() *peerRepMetrics {
	return &peerRepMetrics{
		skippedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: peerRepNamespace,
			Subsystem: peerRepSubsystem,
			Name:      "skipped_total",
			Help:      "BEP-9 attempts the cache suppressed (Enforce=true). Per-class.",
		}, []string{peerRepLabel}),
		wouldSkipTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: peerRepNamespace,
			Subsystem: peerRepSubsystem,
			Name:      "would_skip_total",
			Help:      "Counterfactual: how often the cache WOULD have suppressed the attempt in shadow mode (Enforce=false). Per-class.",
		}, []string{peerRepLabel}),
		observedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: peerRepNamespace,
			Subsystem: peerRepSubsystem,
			Name:      "observed_total",
			Help:      "Outcomes fed back into the cache after an attempted fetch. Class is `success` or one of peerrep.ErrorClass.String().",
		}, []string{peerRepLabel}),
	}
}

func (m *peerRepMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.skippedTotal, m.wouldSkipTotal, m.observedTotal}
}

// labelsFor maps a peerrep.ErrorClass to a prometheus.Labels with the
// class label populated. Centralised so the wrapper + the metrics
// helpers agree on the label key.
func labelsFor(c peerrep.ErrorClass) prometheus.Labels {
	return prometheus.Labels{peerRepLabel: c.String()}
}

// prometheusLabelsFor lets the wrapper pass a literal class string
// (e.g. "success") without round-tripping through ErrorClass.
func prometheusLabelsFor(class string) prometheus.Labels {
	return prometheus.Labels{peerRepLabel: class}
}
