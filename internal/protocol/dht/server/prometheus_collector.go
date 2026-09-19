package server

import (
	"context"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/protocol/dht"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

type prometheusCollector struct {
	queryDuration     *dualemit.HistogramVec
	querySuccessTotal *dualemit.CounterVec
	queryErrorTotal   *dualemit.CounterVec
	queryConcurrency  *dualemit.GaugeVec
	queryLimiterWait  *dualemit.HistogramVec
}

const labelQuery = "query"

var labelNames = []string{labelQuery}

func newPrometheusCollector() prometheusCollector {
	return prometheusCollector{
		queryDuration: dualemit.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "query_duration_seconds",
			Help:      "A histogram of successful DHT query durations in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.1, 1.5, 5),
		}, labelNames),
		querySuccessTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "query_success_total",
			Help:      "A counter of successful DHT queries.",
		}, labelNames),
		queryErrorTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "query_error_total",
			Help:      "A counter of failed DHT queries.",
		}, labelNames),
		queryConcurrency: dualemit.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "query_concurrency",
			Help:      "Number of concurrent DHT queries.",
		}, labelNames),
		// Wait time spent inside the per-remote-IP query rate limiter
		// before a token is issued. A healthy crawler sees this skewed
		// toward 0; a long tail by `query` label identifies which
		// query types are clustering on specific peers and hitting
		// the burst ceiling.
		queryLimiterWait: dualemit.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "query_rate_limit_wait_seconds",
			Help:      "Time spent waiting in the per-remote-IP outbound query rate limiter.",
			// Finer low-end than query_duration: most waits are either
			// ~0 (token immediately available) or ~1s (blocked on
			// sustained rate). Same 5-bucket exp shape for consistency.
			Buckets: prometheus.ExponentialBuckets(0.001, 4, 7),
		}, labelNames),
	}
}

type prometheusServerWrapper struct {
	prometheusCollector
	server Server
}

func (s prometheusServerWrapper) start() error {
	return s.server.start()
}

func (s prometheusServerWrapper) stop() {
	s.server.stop()
}

func (s prometheusServerWrapper) Query(
	ctx context.Context,
	addr netip.AddrPort,
	q string,
	args dht.MsgArgs,
) (dht.RecvMsg, error) {
	labels := prometheus.Labels{labelQuery: q}
	s.queryConcurrency.With(labels).Inc()

	start := time.Now()
	res, err := s.server.Query(ctx, addr, q, args)
	s.queryConcurrency.With(labels).Dec()

	if err == nil {
		s.queryDuration.With(labels).Observe(time.Since(start).Seconds())
		s.querySuccessTotal.With(labels).Inc()
	} else {
		s.queryErrorTotal.With(labels).Inc()
	}

	return res, err
}
