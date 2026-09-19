package metainforequester

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/peerrep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRequester records every Request call. Returns the configured
// (resp, err) for each invocation. Tests use this to verify that the
// wrapper either skipped (calls=0) or fell through (calls=1).
type stubRequester struct {
	calls int
	err   error
}

func (s *stubRequester) Request(_ context.Context, _ protocol.ID, _ netip.AddrPort) (Response, error) {
	s.calls++
	return Response{}, s.err
}

func mkPeer(s string) netip.AddrPort {
	return netip.MustParseAddrPort(s)
}

// TestPeerRepWrapper_DisabledFallsThrough proves the wrapper is a
// total no-op when peerrep is off — the contract for safe deploy.
func TestPeerRepWrapper_DisabledFallsThrough(t *testing.T) {
	cfg := peerrep.NewDefaultConfig()
	cfg.Enabled = false
	store := peerrep.NewStore(cfg, nil)
	stub := &stubRequester{err: nil}
	w := newPeerRepWrapper(stub, store, newPeerRepMetrics())

	for range 5 {
		_, err := w.Request(context.Background(), protocol.ID{}, mkPeer("198.51.100.1:6881"))
		require.NoError(t, err)
	}
	assert.Equal(t, 5, stub.calls,
		"with peerrep disabled, every Request must reach the inner requester")
}

// TestPeerRepWrapper_EnforceTrueSkipsSuppressedPeer is the headline
// behaviour: after a recorded failure, the peer is suppressed for
// schedule[0]. While suppressed, Request returns ErrPeerRepSuppressed
// without touching the inner stack — the whole point of the cache.
func TestPeerRepWrapper_EnforceTrueSkipsSuppressedPeer(t *testing.T) {
	cfg := peerrep.NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.DialTimeoutBackoff = []time.Duration{30 * time.Minute}
	store := peerrep.NewStore(cfg, nil)
	stub := &stubRequester{err: context.DeadlineExceeded}
	w := newPeerRepWrapper(stub, store, newPeerRepMetrics())

	peer := mkPeer("198.51.100.2:6881")

	// First call: timeout, recorded as DialTimeout.
	_, err1 := w.Request(context.Background(), protocol.ID{}, peer)
	require.Error(t, err1)
	require.Equal(t, 1, stub.calls)

	// Second call within suppression window: skipped without dialing.
	_, err2 := w.Request(context.Background(), protocol.ID{}, peer)
	require.ErrorIs(t, err2, ErrPeerRepSuppressed,
		"suppressed peer must surface ErrPeerRepSuppressed sentinel")
	require.Equal(t, 1, stub.calls,
		"inner requester MUST NOT be called for a suppressed peer")
}

// TestPeerRepWrapper_ShadowModeAlwaysFallsThrough is the operational-
// safety contract: even with a peer in active suppression, Enforce=
// false means the inner is still called. The metric counts what
// would have been skipped.
func TestPeerRepWrapper_ShadowModeAlwaysFallsThrough(t *testing.T) {
	cfg := peerrep.NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = false // shadow
	cfg.DialTimeoutBackoff = []time.Duration{30 * time.Minute}
	store := peerrep.NewStore(cfg, nil)
	stub := &stubRequester{err: context.DeadlineExceeded}
	w := newPeerRepWrapper(stub, store, newPeerRepMetrics())

	peer := mkPeer("198.51.100.3:6881")

	// Three failures into shadow mode: every one should reach inner.
	for range 3 {
		_, err := w.Request(context.Background(), protocol.ID{}, peer)
		require.Error(t, err) // the underlying timeout, not the peerrep sentinel
		require.NotErrorIs(t, err, ErrPeerRepSuppressed,
			"shadow mode must NEVER short-circuit — only observe")
	}
	assert.Equal(t, 3, stub.calls,
		"shadow mode must let every call through to the inner requester")
}

// TestPeerRepWrapper_SuccessClearsSuppression — the post-success
// recovery path. A peer that times out then comes back online must
// not stay blacklisted.
func TestPeerRepWrapper_SuccessClearsSuppression(t *testing.T) {
	cfg := peerrep.NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.DialTimeoutBackoff = []time.Duration{1 * time.Hour}
	store := peerrep.NewStore(cfg, nil)
	stub := &stubRequester{err: context.DeadlineExceeded}
	w := newPeerRepWrapper(stub, store, newPeerRepMetrics())

	peer := mkPeer("198.51.100.4:6881")

	// Trigger 1h suppression.
	_, _ = w.Request(context.Background(), protocol.ID{}, peer)
	require.Equal(t, 1, stub.calls)

	// Suppressed.
	_, err := w.Request(context.Background(), protocol.ID{}, peer)
	require.ErrorIs(t, err, ErrPeerRepSuppressed)
	require.Equal(t, 1, stub.calls)

	// Now the peer recovers — flip stub to success and force-clear
	// via direct store call (production: the wrapper itself observes
	// the success and calls RecordSuccess; here we simulate the
	// out-of-band recovery the operator might use).
	stub.err = nil
	store.RecordSuccess(peer)

	_, err = w.Request(context.Background(), protocol.ID{}, peer)
	require.NoError(t, err)
	require.Equal(t, 2, stub.calls,
		"after RecordSuccess clears suppression, the inner is called again")
}

// TestClassifyForPeerRep covers the error-class mapping the wrapper
// uses to feed peerrep.RecordFailure. New error patterns or class
// shifts in the inner requester should be reflected here.
func TestClassifyForPeerRep(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want peerrep.ErrorClass
	}{
		{"nil → unknown", nil, peerrep.ClassUnknown},
		{"context.DeadlineExceeded → DialTimeout", context.DeadlineExceeded, peerrep.ClassDialTimeout},
		{"context.Canceled → unknown (shutdown noise)", context.Canceled, peerrep.ClassUnknown},
		{
			// BEHAVIOUR CHANGE: net.OpError carrying "connection
			// refused" now lands in ConnectionRefused (was
			// NetworkError). Per gpt-5.5-pro 2026-04-25 review —
			// peer-actively-rejecting is structurally distinct from
			// route-unreachable and warrants a longer backoff
			// schedule.
			"net.OpError connection refused → ConnectionRefused",
			&net.OpError{Op: "dial", Err: errors.New("connection refused")},
			peerrep.ClassConnectionRefused,
		},
		{
			"net.OpError without specific message → NetworkError",
			&net.OpError{Op: "read", Err: errors.New("some transport-layer thing")},
			peerrep.ClassNetworkError,
		},
		{
			"timeout marked metadata → MetadataTimeout",
			&timeoutWithMsg{msg: "ut_metadata read timeout"},
			peerrep.ClassMetadataTimeout,
		},
		{
			"plain timeout → DialTimeout",
			&timeoutWithMsg{msg: "i/o timeout"},
			peerrep.ClassDialTimeout,
		},
		{
			"no ut_metadata extension → NoUtMetadata",
			errors.New("peer reply: no ut_metadata extension advertised"),
			peerrep.ClassNoUtMetadata,
		},
		{
			// BEHAVIOUR CHANGE: hash-mismatch was conflated with
			// generic HandshakeFail; now its own class with longer
			// backoff because it indicates a broken/hostile peer
			// for this swarm.
			"info hash mismatch → HashMismatch",
			errors.New("bt handshake: info hash mismatch"),
			peerrep.ClassHashMismatch,
		},
		{
			"handshake without hash mismatch → HandshakeFail",
			errors.New("bt handshake: protocol header mismatch"),
			peerrep.ClassHandshakeFail,
		},
		// New ConnectionRefused / PeerClosed / network-unreachable
		// patterns from the senior-review-driven taxonomy:
		{
			"connection reset by peer → PeerClosed",
			errors.New("read tcp: connection reset by peer"),
			peerrep.ClassPeerClosed,
		},
		{
			"unexpected EOF → PeerClosed",
			errors.New("ext handshake: unexpected EOF"),
			peerrep.ClassPeerClosed,
		},
		{
			"broken pipe → PeerClosed",
			errors.New("write: broken pipe"),
			peerrep.ClassPeerClosed,
		},
		{
			"use of closed network connection → PeerClosed",
			errors.New("read: use of closed network connection"),
			peerrep.ClassPeerClosed,
		},
		{
			"no route to host → NetworkError",
			errors.New("dial tcp: no route to host"),
			peerrep.ClassNetworkError,
		},
		{
			"network unreachable → NetworkError",
			errors.New("connect: network unreachable"),
			peerrep.ClassNetworkError,
		},
		{
			"bare EOF → PeerClosed (peer closed cleanly mid-handshake)",
			errors.New("read EOF"),
			peerrep.ClassPeerClosed,
		},
		{
			"unrecognised → unknown",
			errors.New("transient unknown wire error"),
			peerrep.ClassUnknown,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyForPeerRep(tt.err)
			assert.Equal(t, tt.want, got, "want %v, got %v", tt.want, got)
		})
	}
}

// timeoutWithMsg implements net.Error{Timeout()=true} with a
// configurable message string so tests can drive the metadata-vs-dial
// branch in classifyForPeerRep.
type timeoutWithMsg struct{ msg string }

func (e *timeoutWithMsg) Error() string   { return e.msg }
func (e *timeoutWithMsg) Timeout() bool   { return true }
func (e *timeoutWithMsg) Temporary() bool { return false }
