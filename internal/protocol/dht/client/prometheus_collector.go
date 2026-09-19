package client

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

// Prometheus subsystem + label constants for outbound DHT RPC
// telemetry. Kept distinct from the inbound `dht_server` namespace so
// dashboards don't have to guess which direction a metric describes.
const (
	namespace = "bitagent"
	subsystem = "dht_client"

	labelQuery  = "query"
	labelReason = "reason"
)

// Error-classification buckets for outbound RPC failures. The taxonomy
// stays small on purpose — if an operator can't tell the difference in
// practice, another label just dilutes the signal. Extend only when a
// new class is genuinely actionable.
const (
	reasonTimeout  = "timeout"  // ctx.DeadlineExceeded, net timeout
	reasonCanceled = "canceled" // ctx.Canceled (shutdown; distinguish from real failures)
	reasonKRPC     = "krpc"     // peer replied with a bencoded KRPC error ({"y":"e"})
	reasonNetwork  = "network"  // socket/transport failure (refused, unreachable, EOF, etc.)
	reasonOther    = "other"    // everything we can't confidently classify yet
)

var clientLabelsDuration = []string{labelQuery}

// clientLabelsError carries both `query` and `reason` — without the
// latter an operator has to slice by string-match on log lines to tell
// a timeout from a KRPC-204 "method unknown", which is the work we're
// trying to eliminate.
var clientLabelsError = []string{labelQuery, labelReason}

type prometheusCollector struct {
	requestDuration    *dualemit.HistogramVec
	requestSuccess     *dualemit.CounterVec
	requestError       *dualemit.CounterVec
	requestConcurrency *dualemit.GaugeVec
}

func newPrometheusCollector() prometheusCollector {
	return prometheusCollector{
		requestDuration: dualemit.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "request_duration_seconds",
			Help:      "Histogram of outbound DHT RPC durations in seconds (successes only).",
			// Same exponential shape as the server-side histogram so
			// client + server latency dashboards can share axes.
			Buckets: prometheus.ExponentialBuckets(0.1, 1.5, 5),
		}, clientLabelsDuration),
		requestSuccess: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "request_success_total",
			Help:      "Count of outbound DHT RPCs that returned a non-error response.",
		}, clientLabelsDuration),
		requestError: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "request_error_total",
			Help:      "Count of outbound DHT RPCs that failed, broken down by failure class.",
		}, clientLabelsError),
		requestConcurrency: dualemit.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "request_concurrency",
			Help:      "Number of outbound DHT RPCs currently in flight.",
		}, clientLabelsDuration),
	}
}

// classifyError maps an outbound RPC error to a bounded `reason`
// label value. Errors from the server adapter bubble up wrapped (see
// server.serverError and the underlying socket layer), so we unwrap
// with errors.Is / errors.As rather than string-matching.
func classifyError(err error) string {
	if err == nil {
		return ""
	}

	if errors.Is(err, context.Canceled) {
		return reasonCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return reasonTimeout
	}

	// A KRPC error is the peer explicitly refusing the request (e.g.
	// "method unknown"). Distinguishing it from transport failure is
	// how we'll eventually see BEP-42 rejections separately from
	// routing timeouts.
	var kerr dht.Error
	if errors.As(err, &kerr) {
		return reasonKRPC
	}
	kerrPtr := &dht.Error{}
	if errors.As(err, &kerrPtr) {
		return reasonKRPC
	}

	// net.Error with Timeout() true also counts as a timeout even if
	// it didn't come through context.DeadlineExceeded (e.g. per-socket
	// deadline).
	type timeoutError interface{ Timeout() bool }
	var te timeoutError
	if errors.As(err, &te) && te.Timeout() {
		return reasonTimeout
	}

	// Anything with Network() semantics: refused, unreachable, EOF,
	// malformed wire. If we later find one of these is common enough
	// to split out, add a label value — don't split the metric.
	type netErrorLike interface{ Error() string }
	if _, ok := err.(netErrorLike); ok {
		return reasonNetwork
	}

	return reasonOther
}

// prometheusClientWrapper instruments an inner Client. Every call
// records concurrency + duration + success/failure + failure class
// with zero retry / zero policy of its own; it's a passive observer.
type prometheusClientWrapper struct {
	prometheusCollector
	inner Client
}

// trackInflight Inc()s the concurrency gauge and returns a deferred
// finalizer that Dec()s it AND records duration / success / error.
//
// Returning a closure (rather than a bare `start time.Time`) lets each
// caller `defer finalize(&err)` so a panic in the inner client unwinds
// the gauge correctly. Pre-2026-04-24 the bare `before(...) ... observe(...)`
// pattern relied on observe() running after every Ping/FindNode/etc;
// any panic in the inner stack leaked the gauge by +1 permanently.
func (p prometheusClientWrapper) trackInflight(query string) (start time.Time, finalize func(*error)) {
	dLabels := prometheus.Labels{labelQuery: query}
	p.requestConcurrency.With(dLabels).Inc()
	start = time.Now()
	finalize = func(errp *error) {
		p.requestConcurrency.With(dLabels).Dec()
		var err error
		if errp != nil {
			err = *errp
		}
		if err == nil {
			p.requestDuration.With(dLabels).Observe(time.Since(start).Seconds())
			p.requestSuccess.With(dLabels).Inc()
			return
		}
		p.requestError.With(prometheus.Labels{
			labelQuery:  query,
			labelReason: classifyError(err),
		}).Inc()
	}
	return start, finalize
}

func (p prometheusClientWrapper) Ping(ctx context.Context, addr netip.AddrPort) (res PingResult, err error) {
	_, finalize := p.trackInflight(dht.QPing)
	defer finalize(&err)
	res, err = p.inner.Ping(ctx, addr)
	return res, err
}

func (p prometheusClientWrapper) FindNode(
	ctx context.Context,
	addr netip.AddrPort,
	target protocol.ID,
) (res FindNodeResult, err error) {
	_, finalize := p.trackInflight(dht.QFindNode)
	defer finalize(&err)
	res, err = p.inner.FindNode(ctx, addr, target)
	return res, err
}

func (p prometheusClientWrapper) GetPeers(
	ctx context.Context,
	addr netip.AddrPort,
	infoHash protocol.ID,
) (res GetPeersResult, err error) {
	_, finalize := p.trackInflight(dht.QGetPeers)
	defer finalize(&err)
	res, err = p.inner.GetPeers(ctx, addr, infoHash)
	return res, err
}

func (p prometheusClientWrapper) GetPeersScrape(
	ctx context.Context,
	addr netip.AddrPort,
	infoHash protocol.ID,
) (res GetPeersScrapeResult, err error) {
	// Distinct label so the dashboards can separate scrape-bearing
	// get_peers from the plain variant — the call cost is materially
	// different and mixing them hides regressions.
	const q = dht.QGetPeers + ":scrape"
	_, finalize := p.trackInflight(q)
	defer finalize(&err)
	res, err = p.inner.GetPeersScrape(ctx, addr, infoHash)
	return res, err
}

func (p prometheusClientWrapper) SampleInfoHashes(
	ctx context.Context,
	addr netip.AddrPort,
	target protocol.ID,
) (res SampleInfoHashesResult, err error) {
	_, finalize := p.trackInflight(dht.QSampleInfohashes)
	defer finalize(&err)
	res, err = p.inner.SampleInfoHashes(ctx, addr, target)
	return res, err
}
