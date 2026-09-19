package metainforequester

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/peerrep"
)

// peerRepWrapper layers per-peer reputation skipping over an inner
// Requester. Sits OUTSIDE the rate limiter (cheaper to skip a peer
// entirely than to wait for a rate-limit token before making an
// already-doomed call).
//
// In shadow mode (`peerrep.Config.Enforce=false`), skip decisions are
// observed but every Request still falls through to the inner stack —
// the metric tells operators what enforce mode would have done.
//
// Outcomes feed the cache via RecordSuccess / RecordFailure(class).
// Classification of a failure is a best-effort mapping from the inner
// requester's error chain; ambiguous errors map to ClassUnknown which
// uses the (conservative) NetworkErrorBackoff schedule.
type peerRepWrapper struct {
	requester Requester
	store     *peerrep.Store
	metrics   *peerRepMetrics
}

func newPeerRepWrapper(inner Requester, store *peerrep.Store, metrics *peerRepMetrics) Requester {
	return peerRepWrapper{requester: inner, store: store, metrics: metrics}
}

func (p peerRepWrapper) Request(ctx context.Context, infoHash protocol.ID, addr netip.AddrPort) (Response, error) {
	dec := p.store.Decide(addr)

	// Always observe the decision. In shadow mode (WouldSkip=true,
	// Allow=true) the counter still increments — that's the whole
	// point of shadow mode.
	if dec.WouldSkip {
		p.metrics.wouldSkipTotal.With(labelsFor(dec.LastClass)).Inc()
	}
	if !dec.Allow {
		p.metrics.skippedTotal.With(labelsFor(dec.LastClass)).Inc()
		// Surface a sentinel error so the outer worker can move on
		// to the next peer without delay. The crawler's
		// doRequestMetaInfo loop catches errors and continues —
		// errors.Is(err, ErrPeerRepSuppressed) lets a future call
		// site distinguish suppression from real network failure.
		return Response{}, ErrPeerRepSuppressed
	}

	resp, err := p.requester.Request(ctx, infoHash, addr)
	if err == nil {
		p.store.RecordSuccess(addr)
		p.metrics.observedTotal.With(prometheusLabelsFor("success")).Inc()
		return resp, nil
	}

	class := classifyForPeerRep(err)
	p.store.RecordFailure(addr, class)
	p.metrics.observedTotal.With(prometheusLabelsFor(class.String())).Inc()
	return resp, err
}

// ErrPeerRepSuppressed is returned when peerrep decided this peer is in
// active backoff. Distinct from network/timeout errors so callers can
// tell "we skipped" from "we tried and failed."
var ErrPeerRepSuppressed = errors.New("peerrep: peer suppressed by reputation cache")

// classifyForPeerRep maps the inner requester's error chain to a
// peerrep.ErrorClass. Best-effort: when in doubt, returns
// ClassUnknown (which uses NetworkErrorBackoff in the cache).
//
// The mapping reflects the BEP-9 fetch chain in
// metainforequester/requester.go:
//   - dial step deadline   → context.DeadlineExceeded → DialTimeout
//   - dial step refused    → "connection refused"     → ConnectionRefused
//   - tcp closed mid-flight → "EOF" / "broken pipe" / "reset by peer" → PeerClosed
//   - tcp connected, BT handshake fails → HandshakeFail
//   - peer responded with wrong info_hash → HashMismatch
//     (split out of HandshakeFail per gpt-5.5-pro 2026-04-25 review)
//   - extension protocol absent → NoUtMetadata
//   - metadata read times out after handshake → MetadataTimeout
//   - peer sends explicit error message → KRPCError
//
// The discriminators below are duplicated from requester.go because
// that package keeps its errors as plain `errors.New(...)` strings;
// upstreaming them to typed errors would make this cleaner but is
// out of scope for this MR.
//
// Order matters: more-specific patterns win over less-specific.
// `connection refused` BEFORE generic `OpError`; `info hash mismatch`
// BEFORE generic `handshake`; `EOF` / `reset by peer` BEFORE generic
// `network error`.
func classifyForPeerRep(err error) peerrep.ErrorClass {
	if err == nil {
		return peerrep.ClassUnknown
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return peerrep.ClassDialTimeout
	}
	if errors.Is(err, context.Canceled) {
		return peerrep.ClassUnknown
	}

	msg := err.Error()

	// More-specific patterns first — these win over the generic
	// OpError / Timeout fallbacks below.
	switch {
	case containsAny(msg, "info hash mismatch", "wrong info hash", "info_hash mismatch"):
		// Peer's BEP-9 reply was for a different swarm. Protocol
		// error or attack. Distinct from generic handshake fail.
		return peerrep.ClassHashMismatch
	case containsAny(msg, "connection refused"):
		// TCP RST on connect. Distinct from DialTimeout: peer
		// is reachable but actively rejecting.
		return peerrep.ClassConnectionRefused
	case containsAny(msg,
		"connection reset by peer",
		"broken pipe",
		"unexpected EOF",
		"use of closed network connection",
		"connection closed"):
		// TCP closed mid-flight. Peer was willing to connect at
		// least once but the connection died.
		return peerrep.ClassPeerClosed
	case containsAny(msg, "no ut_metadata", "extension not supported", "no extension"):
		return peerrep.ClassNoUtMetadata
	case containsAny(msg, "no route to host", "network unreachable", "host unreachable"):
		return peerrep.ClassNetworkError
	}

	type timeoutError interface{ Timeout() bool }
	var te timeoutError
	if errors.As(err, &te) && te.Timeout() {
		// Net-level timeout. Could be at dial OR at metadata-read
		// stage; without typed errors from the inner we can't tell.
		// Map to MetadataTimeout if the message hints at metadata,
		// else DialTimeout.
		if containsAny(msg, "metadata", "ut_metadata", "ext", "extended") {
			return peerrep.ClassMetadataTimeout
		}
		return peerrep.ClassDialTimeout
	}

	var netOpErr *net.OpError
	if errors.As(err, &netOpErr) {
		// Generic OpError without a specific message we recognise.
		// Network-class is the safe default.
		return peerrep.ClassNetworkError
	}

	// Final string-match fallbacks.
	switch {
	case containsAny(msg, "handshake", "bt handshake"):
		return peerrep.ClassHandshakeFail
	case containsAny(msg, "metadata", "piece", "ut_metadata"):
		return peerrep.ClassMetadataTimeout
	case containsAny(msg, "EOF"):
		// Bare EOF without "unexpected" — usually a peer that
		// closed cleanly mid-extension-handshake.
		return peerrep.ClassPeerClosed
	}

	return peerrep.ClassUnknown
}

// containsAny reports whether s contains any of the substrings. No
// regex needed — patterns are short and bounded.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

// indexOf is strings.Index without an import — keeps this file's
// transitive imports tight. The classifier is on the hot path of
// every BEP-9 reply; a strings import is fine but explicit-cheap is
// fine too.
func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(s) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
