package peerrep

import (
	"net/netip"
	"time"
)

// Decision is what the cache tells the caller for one attempt.
type Decision struct {
	// Allow: caller should make the BEP-9 attempt.
	// false → caller should skip (under enforce); WouldSkip captures
	// what enforce-mode would have done.
	Allow bool

	// WouldSkip is true iff the peer is currently in active backoff.
	// Always set; in shadow mode (Enforce=false) Allow stays true
	// while WouldSkip lights up — that's how we measure the cache's
	// hypothetical impact before flipping enforcement.
	WouldSkip bool

	// LastClass is the most recent failure classification (for log /
	// metric labels). Zero-valued (ClassUnknown) when the peer has
	// no history yet.
	LastClass ErrorClass
}

// Decide returns whether the caller should attempt the peer right now.
// It does not mutate the cache (cache is only updated via RecordSuccess
// / RecordFailure). Safe to call without prior state.
func (s *Store) Decide(peer netip.AddrPort) Decision {
	if !s.cfg.Enabled {
		return Decision{Allow: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.peek(peer)
	if st == nil || st.suppressedUntil.IsZero() {
		return Decision{Allow: true}
	}
	now := s.clock()
	if now.Before(st.suppressedUntil) {
		// Active suppression. In shadow mode, still allow but
		// flag would-skip.
		d := Decision{
			WouldSkip: true,
			LastClass: st.lastClass,
		}
		d.Allow = !s.cfg.Enforce
		return d
	}
	// Suppression window elapsed; allow + clear so the next failure
	// starts the count over from a clean slate.
	st.suppressedUntil = time.Time{}
	return Decision{Allow: true, LastClass: st.lastClass}
}

// RecordSuccess clears any failure state and active suppression for
// the given peer. The next failure starts the consecutive counter
// from 1 — successes always reset.
func (s *Store) RecordSuccess(peer netip.AddrPort) {
	if !s.cfg.Enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.upsert(peer)
	st.consecutiveFailures = 0
	st.suppressedUntil = time.Time{}
	// LastClass left as-is — the metric / log path may still want it.
}

// RecordFailure increments the consecutive-failure counter for the
// peer + sets suppressedUntil per the schedule for the given class.
func (s *Store) RecordFailure(peer netip.AddrPort, class ErrorClass) {
	if !s.cfg.Enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.upsert(peer)
	// Class change (e.g. dial-timeout → handshake-fail after the
	// peer comes back up) resets the run-length so the new class's
	// schedule starts at index 0.
	if class != st.lastClass {
		st.consecutiveFailures = 0
		st.lastClass = class
	}
	st.consecutiveFailures++

	schedule := s.scheduleFor(class)
	if len(schedule) == 0 {
		st.suppressedUntil = time.Time{}
		return
	}
	idx := st.consecutiveFailures - 1
	if idx >= len(schedule) {
		idx = len(schedule) - 1
	}
	st.suppressedUntil = s.clock().Add(schedule[idx])
}

// scheduleFor returns the configured backoff list for an error class.
// Falls back to NetworkErrorBackoff for ClassUnknown OR for any new
// class whose backoff config is unset (zero-len slice). The unset
// fallback matters for forward-compat: if a future class's backoff
// is added to ErrorClass but the operator's deployed config predates
// it, the cache still works rather than silently never suppressing.
func (s *Store) scheduleFor(class ErrorClass) []time.Duration {
	var sched []time.Duration
	switch class {
	case ClassDialTimeout:
		sched = s.cfg.DialTimeoutBackoff
	case ClassConnectionRefused:
		sched = s.cfg.ConnectionRefusedBackoff
	case ClassNetworkError:
		sched = s.cfg.NetworkErrorBackoff
	case ClassPeerClosed:
		sched = s.cfg.PeerClosedBackoff
	case ClassHandshakeFail:
		sched = s.cfg.HandshakeFailBackoff
	case ClassHashMismatch:
		sched = s.cfg.HashMismatchBackoff
	case ClassNoUtMetadata:
		sched = s.cfg.NoUtMetadataBackoff
	case ClassMetadataTimeout:
		sched = s.cfg.MetadataTimeoutBackoff
	case ClassKRPCError:
		sched = s.cfg.KRPCErrorBackoff
	}
	if len(sched) > 0 {
		return sched
	}
	return s.cfg.NetworkErrorBackoff
}
