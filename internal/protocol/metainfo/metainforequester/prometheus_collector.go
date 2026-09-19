package metainforequester

import (
	"context"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

type prometheusCollector struct {
	requester           Requester
	requestDuration     *dualemit.Histogram
	requestSuccessTotal *dualemit.Counter
	requestErrorTotal   *dualemit.Counter
	requestConcurrency  *dualemit.Gauge
}

const (
	namespace = "bitagent"
	subsystem = "meta_info_requester"
)

func newPrometheusCollector(requester Requester) *prometheusCollector {
	return &prometheusCollector{
		requester: requester,
		requestDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "duration_seconds",
			Help:      "Duration of successful meta info requests in seconds.",
			Buckets:   prometheus.DefBuckets,
		}),
		requestSuccessTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "success_total",
			Help:      "Total number of successful meta info requests.",
		}),
		requestErrorTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "error_total",
			Help:      "Total number of failed meta info requests.",
		}),
		requestConcurrency: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "concurrency",
			Help:      "Number of concurrent meta info requests.",
		}),
	}
}

func (l prometheusCollector) Request(ctx context.Context, infoHash protocol.ID, addr netip.AddrPort) (Response, error) {
	l.requestConcurrency.Inc()
	// Defer the Dec so a panic in the inner stack (TCP dial, BEP-10
	// extension handshake, BEP-9 metadata read) does not leak the gauge
	// permanently. Pre-2026-04-24 the bare `Inc(); ...; Dec()` pattern
	// caused a slow positive drift on `meta_info_requester_concurrency`
	// (observed 570 vs configured-cap 400 on the reference deployment) every time an
	// inner code path panicked even rarely.
	defer l.requestConcurrency.Dec()

	start := time.Now()
	resp, err := l.requester.Request(ctx, infoHash, addr)

	if err == nil {
		l.requestDuration.Observe(time.Since(start).Seconds())
		l.requestSuccessTotal.Inc()
	} else {
		l.requestErrorTotal.Inc()
	}

	return resp, err
}
