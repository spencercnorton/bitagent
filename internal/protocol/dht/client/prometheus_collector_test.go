package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClient lets tests control each RPC's return. All methods share
// the same (value, err) channel so the test can script a sequence.
type fakeClient struct {
	err error
}

func (f fakeClient) Ping(context.Context, netip.AddrPort) (PingResult, error) {
	return PingResult{}, f.err
}

func (f fakeClient) FindNode(context.Context, netip.AddrPort, protocol.ID) (FindNodeResult, error) {
	return FindNodeResult{}, f.err
}

func (f fakeClient) GetPeers(context.Context, netip.AddrPort, protocol.ID) (GetPeersResult, error) {
	return GetPeersResult{}, f.err
}

func (f fakeClient) GetPeersScrape(context.Context, netip.AddrPort, protocol.ID) (GetPeersScrapeResult, error) {
	return GetPeersScrapeResult{}, f.err
}

func (f fakeClient) SampleInfoHashes(
	context.Context, netip.AddrPort, protocol.ID,
) (SampleInfoHashesResult, error) {
	return SampleInfoHashesResult{}, f.err
}

func TestClassifyError(t *testing.T) {
	t.Parallel()

	// A fake net.Error with Timeout() true — a per-socket deadline
	// surfaces this way, separate from context.DeadlineExceeded.
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil returns empty", nil, ""},
		{"context.Canceled", context.Canceled, reasonCanceled},
		{"context.DeadlineExceeded", context.DeadlineExceeded, reasonTimeout},
		{"wrapped DeadlineExceeded", fmt.Errorf("query: %w", context.DeadlineExceeded), reasonTimeout},
		{"KRPC error value",
			dht.Error{Code: dht.ErrorCodeMethodUnknown, Msg: "method unknown"},
			reasonKRPC},
		{"wrapped KRPC error",
			fmt.Errorf("query failed: %w", dht.Error{Code: dht.ErrorCodeProtocolError, Msg: "nope"}),
			reasonKRPC},
		{"net.Error-with-Timeout",
			&net.OpError{Op: "read", Err: timeoutNetErr{}},
			reasonTimeout},
		{"plain errors.New is network-ish",
			errors.New("connection reset"),
			reasonNetwork},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, classifyError(tt.err))
		})
	}
}

// timeoutNetErr implements net.Error with Timeout()==true so the
// classifier can match by interface.
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

// counterValue extracts the current value of a CounterVec child for a
// given label set. Returns 0 when the child doesn't exist yet.
func counterValue(t *testing.T, vec *dualemit.CounterVec, lvs ...string) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, vec.WithLabelValues(lvs...).Write(&m))
	if m.Counter == nil {
		return 0
	}

	return m.Counter.GetValue()
}

func TestPrometheusClientWrapper_successRecordsDurationAndCounter(t *testing.T) {
	t.Parallel()

	collector := newPrometheusCollector()
	wrapped := prometheusClientWrapper{
		prometheusCollector: collector,
		inner:               fakeClient{err: nil},
	}

	addr := netip.MustParseAddrPort("198.51.100.1:6881")
	_, err := wrapped.Ping(context.Background(), addr)
	require.NoError(t, err)

	assert.InDelta(t, 1.0,
		counterValue(t, collector.requestSuccess, dht.QPing), 0.001,
		"success counter should be exactly 1 after one successful call")

	// Failure counter must stay flat — no label drift / double-count.
	assert.InDelta(t, 0.0,
		counterValue(t, collector.requestError, dht.QPing, reasonTimeout), 0.001)
	assert.InDelta(t, 0.0,
		counterValue(t, collector.requestError, dht.QPing, reasonNetwork), 0.001)
}

func TestPrometheusClientWrapper_errorClassifiedIntoReason(t *testing.T) {
	t.Parallel()

	collector := newPrometheusCollector()
	wrapped := prometheusClientWrapper{
		prometheusCollector: collector,
		inner:               fakeClient{err: context.DeadlineExceeded},
	}

	addr := netip.MustParseAddrPort("198.51.100.2:6881")
	_, _ = wrapped.FindNode(context.Background(), addr, protocol.RandomNodeID())

	assert.InDelta(t, 1.0,
		counterValue(t, collector.requestError, dht.QFindNode, reasonTimeout), 0.001,
		"timeout should land in the `timeout` reason bucket")
	// Must not leak into other reason buckets.
	assert.InDelta(t, 0.0,
		counterValue(t, collector.requestError, dht.QFindNode, reasonOther), 0.001)
	assert.InDelta(t, 0.0,
		counterValue(t, collector.requestError, dht.QFindNode, reasonKRPC), 0.001)
	// And must not increment success.
	assert.InDelta(t, 0.0,
		counterValue(t, collector.requestSuccess, dht.QFindNode), 0.001)
}

func TestPrometheusClientWrapper_krpcErrorClassifiedAsKRPC(t *testing.T) {
	t.Parallel()

	collector := newPrometheusCollector()
	// A peer responding with KRPC "method unknown" (e.g. no BEP-51
	// support) is a common case — it MUST be distinguishable from a
	// transport timeout, otherwise we can't tell "peer refused" from
	// "peer silent" in dashboards.
	wrapped := prometheusClientWrapper{
		prometheusCollector: collector,
		inner: fakeClient{err: dht.Error{
			Code: dht.ErrorCodeMethodUnknown,
			Msg:  "method unknown",
		}},
	}

	addr := netip.MustParseAddrPort("198.51.100.3:6881")
	_, _ = wrapped.SampleInfoHashes(context.Background(), addr, protocol.RandomNodeID())

	assert.InDelta(t, 1.0,
		counterValue(t, collector.requestError, dht.QSampleInfohashes, reasonKRPC), 0.001)
}

func TestPrometheusClientWrapper_concurrencyGoesUpAndBackDown(t *testing.T) {
	t.Parallel()

	collector := newPrometheusCollector()

	// Block the inner call on a channel so we can observe concurrency
	// == 1 while it's in flight, then release and assert it decrements.
	release := make(chan struct{})
	blocked := make(chan struct{})
	inner := blockingClient{fakeClient: fakeClient{}, release: release, entered: blocked}

	wrapped := prometheusClientWrapper{
		prometheusCollector: collector,
		inner:               inner,
	}

	done := make(chan struct{})
	go func() {
		_, _ = wrapped.GetPeers(context.Background(),
			netip.MustParseAddrPort("198.51.100.4:6881"),
			protocol.RandomNodeID())
		close(done)
	}()

	<-blocked
	var m dto.Metric
	require.NoError(t, collector.requestConcurrency.WithLabelValues(dht.QGetPeers).Write(&m))
	assert.InDelta(t, 1.0, m.Gauge.GetValue(), 0.001,
		"one in-flight call should read as concurrency=1")

	close(release)
	<-done

	require.NoError(t, collector.requestConcurrency.WithLabelValues(dht.QGetPeers).Write(&m))
	assert.InDelta(t, 0.0, m.Gauge.GetValue(), 0.001,
		"concurrency must return to 0 after the call completes")
}

type blockingClient struct {
	fakeClient
	release <-chan struct{}
	entered chan<- struct{}
}

func (b blockingClient) GetPeers(ctx context.Context, _ netip.AddrPort, _ protocol.ID) (GetPeersResult, error) {
	close(b.entered)
	select {
	case <-b.release:
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
	}

	return GetPeersResult{}, nil
}

// panickyClient panics on the first call. Used to verify that the
// concurrency gauge is decremented via defer rather than a bare Dec()
// after the inner call returns. Without `defer`, a panic in the inner
// stack (TCP/BEP-10/BEP-9 in production) would leak the gauge by +1
// permanently — a slow positive drift was the observed cause of the
// 2026-04-24 audit's `meta_info_requester_concurrency=570 vs 400`
// mystery on the reference deployment.
type panickyClient struct{ fakeClient }

func (panickyClient) Ping(context.Context, netip.AddrPort) (PingResult, error) {
	panic("simulated inner Ping panic")
}

func TestPrometheusClientWrapper_concurrencyDecrementsOnPanic(t *testing.T) {
	t.Parallel()

	collector := newPrometheusCollector()
	wrapped := prometheusClientWrapper{
		prometheusCollector: collector,
		inner:               panickyClient{},
	}

	// Recover here, since the panic is intentionally allowed to
	// propagate past the wrapper — the defer-based finalize is the
	// thing under test, not panic suppression.
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate out of the wrapper, got none")
			}
		}()
		addr := netip.MustParseAddrPort("198.51.100.5:6881")
		_, _ = wrapped.Ping(context.Background(), addr)
	}()

	var m dto.Metric
	require.NoError(t, collector.requestConcurrency.WithLabelValues(dht.QPing).Write(&m))
	assert.InDelta(t, 0.0, m.Gauge.GetValue(), 0.001,
		"concurrency gauge MUST return to 0 even when the inner call panics")
}
