package server

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/spencercnorton/bitagent/internal/concurrency"
	"github.com/spencercnorton/bitagent/internal/protocol/dht"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

// stubServer is a minimal Server that returns immediately. Keeps the
// limiter-test scope to the rate-limit path without dragging in the
// real UDP socket.
type stubServer struct {
	calls int
}

func (s *stubServer) start() error { return nil }
func (s *stubServer) stop()        {}
func (s *stubServer) Query(
	_ context.Context,
	_ netip.AddrPort,
	_ string,
	_ dht.MsgArgs,
) (dht.RecvMsg, error) {
	s.calls++
	return dht.RecvMsg{}, nil
}

// sumHistogram returns the total sample count across every label combo
// currently attached to the vec. Enough to prove the limiter observed
// at least one sample per query it allowed through.
func sumHistogram(t *testing.T, vec *dualemit.HistogramVec) uint64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	vec.Collect(ch)
	close(ch)
	var total uint64
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		if pb.Histogram != nil {
			total += pb.Histogram.GetSampleCount()
		}
	}
	return total
}

func TestQueryLimiter_ObservesWaitTime(t *testing.T) {
	// Pin dualemit's legacy off for this test so sumHistogram reports
	// the logical observation count rather than 2× (primary + legacy).
	// The dual-emit path is covered by dualemit's own tests.
	prev := dualemit.EmitLegacy
	dualemit.EmitLegacy = false
	t.Cleanup(func() { dualemit.EmitLegacy = prev })

	stub := &stubServer{}
	collector := newPrometheusCollector()
	addr := netip.MustParseAddrPort("203.0.113.1:6881")

	// Generous burst so neither call blocks — we're asserting we
	// observed the wait time, not testing the underlying limiter's
	// own correctness (that's golang.org/x/time/rate's job).
	l := queryLimiter{
		server:       stub,
		queryLimiter: concurrency.NewKeyedLimiter(rate.Every(time.Second), 4, 16, time.Minute),
		waitHist:     collector.queryLimiterWait,
	}

	for range 3 {
		_, err := l.Query(context.Background(), addr, dht.QPing, dht.MsgArgs{})
		require.NoError(t, err)
	}

	assert.Equal(t, 3, stub.calls, "inner server should be invoked once per allowed query")
	assert.Equal(t, uint64(3), sumHistogram(t, collector.queryLimiterWait),
		"every allowed query should record one wait-time sample")
}

// TestQueryLimiter_RespectsContextCancel proves the wait-time histogram
// is NOT observed when the limiter returns a ctx error, so a burst of
// cancellations during shutdown doesn't pollute the wait-time signal.
func TestQueryLimiter_RespectsContextCancel(t *testing.T) {
	stub := &stubServer{}
	collector := newPrometheusCollector()
	addr := netip.MustParseAddrPort("203.0.113.2:6881")

	// burst=0 so Wait() will block on the first token, which never
	// arrives because we immediately cancel the context.
	l := queryLimiter{
		server:       stub,
		queryLimiter: concurrency.NewKeyedLimiter(rate.Every(time.Hour), 0, 16, time.Minute),
		waitHist:     collector.queryLimiterWait,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := l.Query(ctx, addr, dht.QPing, dht.MsgArgs{})

	require.Error(t, err)
	assert.Equal(t, 0, stub.calls, "inner server must not be called when limiter returns error")
	assert.Equal(t, uint64(0), sumHistogram(t, collector.queryLimiterWait),
		"aborted waits must not contribute to the wait histogram")
}
