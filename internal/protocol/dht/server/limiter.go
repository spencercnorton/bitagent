package server

import (
	"context"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/concurrency"
	"github.com/spencercnorton/bitagent/internal/protocol/dht"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
)

type queryLimiter struct {
	server       Server
	queryLimiter concurrency.KeyedLimiter
	waitHist     *dualemit.HistogramVec
}

func (s queryLimiter) start() error {
	return s.server.start()
}

func (s queryLimiter) stop() {
	s.server.stop()
}

func (s queryLimiter) Query(
	ctx context.Context,
	addr netip.AddrPort,
	q string,
	args dht.MsgArgs,
) (r dht.RecvMsg, err error) {
	waitStart := time.Now()
	if limitErr := s.queryLimiter.Wait(ctx, addr.Addr().String()); limitErr != nil {
		return r, limitErr
	}
	if s.waitHist != nil {
		s.waitHist.With(prometheus.Labels{labelQuery: q}).Observe(time.Since(waitStart).Seconds())
	}

	return s.server.Query(ctx, addr, q, args)
}
