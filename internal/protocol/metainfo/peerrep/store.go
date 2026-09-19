package peerrep

import (
	"net/netip"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/simplelru"
)

// peerState tracks one peer's recent BEP-9 history. Mutated under the
// store's mutex, which protects both the non-thread-safe LRU and the
// per-entry reads/writes so the "increment-and-decide" path is atomic.
type peerState struct {
	// failures-of-this-class run length. Reset on success.
	consecutiveFailures int
	// the class of the most recent failure — selects the backoff
	// schedule on the next attempt.
	lastClass ErrorClass
	// suppressedUntil is the earliest time at which the next
	// attempt is allowed under enforce mode. Zero value = no
	// active suppression.
	suppressedUntil time.Time
	// expiresAt preserves the old expirable-LRU contract without its
	// 100 sharded expiry maps. Expiry is checked lazily on access; the
	// size-bounded LRU evicts cold expired entries even if they are never
	// accessed again.
	expiresAt time.Time
}

// Store is a thread-safe LRU keyed on `netip.AddrPort` carrying each
// peer's most recent BEP-9 history. The reputation logic lives in
// `Decide` (decision.go); this file is just storage.
type Store struct {
	cfg   Config
	lru   *lru.LRU[netip.AddrPort, *peerState]
	mu    sync.Mutex
	clock func() time.Time
}

// NewStore builds a store from cfg. The `now` arg lets tests pin a
// deterministic clock; pass `nil` in production for `time.Now`.
func NewStore(cfg Config, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	// simplelru rejects non-positive sizes. More importantly, accepting a
	// zero here must never turn a config mistake into an unbounded cache.
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = defaultMaxEntries
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	cache, err := lru.NewLRU[netip.AddrPort, *peerState](cfg.MaxEntries, nil)
	if err != nil {
		// The guards above make this an internal invariant failure rather
		// than an operator-controlled path. Keep the existing constructor
		// signature and fail loudly if the dependency contract changes.
		panic("peerrep: create bounded LRU: " + err.Error())
	}
	return &Store{
		cfg:   cfg,
		lru:   cache,
		clock: now,
	}
}

// peek returns the entry without bumping LRU recency. Caller must
// hold s.mu. Returns nil if absent.
func (s *Store) peek(key netip.AddrPort) *peerState {
	v, ok := s.lru.Peek(key)
	if !ok {
		return nil
	}
	if s.clock().After(v.expiresAt) {
		s.lru.Remove(key)
		return nil
	}
	return v
}

// upsert returns the existing entry or creates a fresh one. Caller
// must hold s.mu.
func (s *Store) upsert(key netip.AddrPort) *peerState {
	now := s.clock()
	if v, ok := s.lru.Get(key); ok {
		if !now.After(v.expiresAt) {
			return v
		}
		s.lru.Remove(key)
	}
	st := &peerState{expiresAt: now.Add(s.cfg.TTL)}
	s.lru.Add(key, st)
	return st
}

// Len returns the current number of tracked peers — primarily for
// metrics + tests.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lru.Len()
}
