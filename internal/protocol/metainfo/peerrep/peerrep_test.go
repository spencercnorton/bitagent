package peerrep

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func mkPeer(s string) netip.AddrPort {
	return netip.MustParseAddrPort(s)
}

// fakeClock is a controllable monotonic time source for tests. The
// Set method is the only mutator — pin the clock, exercise Decide /
// Record*, advance, repeat.
type fakeClock struct{ now time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time           { return c.now }
func (c *fakeClock) Advance(d time.Duration)  { c.now = c.now.Add(d) }

// --- core lifecycle ------------------------------------------------

func TestDecide_DisabledAllowsEverything(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = false
	s := NewStore(cfg, nil)

	// Even after a recorded failure, disabled mode never blocks and
	// never mutates state — the count must stay at 0.
	peer := mkPeer("198.51.100.1:6881")
	s.RecordFailure(peer, ClassDialTimeout)
	d := s.Decide(peer)
	assert.True(t, d.Allow)
	assert.False(t, d.WouldSkip)
	assert.Equal(t, 0, s.Len(), "disabled store must not store entries")
}

func TestDecide_FreshPeerAllowed(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)

	d := s.Decide(mkPeer("198.51.100.2:6881"))
	assert.True(t, d.Allow, "an unseen peer must always be allowed")
	assert.False(t, d.WouldSkip)
}

// --- record + decide pairing --------------------------------------

func TestRecordFailure_FirstSuppressionMatchesSchedule(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.DialTimeoutBackoff = []time.Duration{5 * time.Minute, 30 * time.Minute}
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)
	peer := mkPeer("198.51.100.3:6881")

	s.RecordFailure(peer, ClassDialTimeout)

	// Within window: skip.
	clk.Advance(4 * time.Minute)
	d := s.Decide(peer)
	assert.False(t, d.Allow, "must skip during active backoff")
	assert.True(t, d.WouldSkip)
	assert.Equal(t, ClassDialTimeout, d.LastClass)

	// After window: allow + state reset.
	clk.Advance(2 * time.Minute) // total 6 min > 5 min schedule
	d = s.Decide(peer)
	assert.True(t, d.Allow, "must allow once suppression window has passed")
	assert.False(t, d.WouldSkip)
}

func TestRecordFailure_StagedBackoffRampsCorrectly(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.DialTimeoutBackoff = []time.Duration{
		5 * time.Minute,
		30 * time.Minute,
		2 * time.Hour,
	}
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)
	peer := mkPeer("198.51.100.4:6881")

	// 1st failure → 5m suppression
	s.RecordFailure(peer, ClassDialTimeout)
	clk.Advance(6 * time.Minute) // window expires
	assert.True(t, s.Decide(peer).Allow)

	// 2nd failure → 30m suppression
	s.RecordFailure(peer, ClassDialTimeout)
	clk.Advance(20 * time.Minute)
	assert.False(t, s.Decide(peer).Allow,
		"second failure should still be in backoff after 20m of a 30m schedule")
	clk.Advance(15 * time.Minute) // total 35m > 30m
	assert.True(t, s.Decide(peer).Allow)

	// 3rd failure → 2h
	s.RecordFailure(peer, ClassDialTimeout)
	clk.Advance(90 * time.Minute)
	assert.False(t, s.Decide(peer).Allow)

	// 4th failure (beyond schedule length) caps at last entry (2h).
	s.RecordFailure(peer, ClassDialTimeout)
	clk.Advance(90 * time.Minute) // total 90m of 2h schedule
	assert.False(t, s.Decide(peer).Allow,
		"oversaturated failure count must cap at the last scheduled value")
}

func TestRecordSuccess_ClearsAllState(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.DialTimeoutBackoff = []time.Duration{1 * time.Hour}
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)
	peer := mkPeer("198.51.100.5:6881")

	s.RecordFailure(peer, ClassDialTimeout)
	assert.False(t, s.Decide(peer).Allow, "must be in suppression window")

	s.RecordSuccess(peer)
	assert.True(t, s.Decide(peer).Allow,
		"success must clear suppression IMMEDIATELY, even mid-window")

	// And the next failure restarts at index 0 of the schedule, not
	// where the previous run left off.
	s.RecordFailure(peer, ClassDialTimeout)
	clk.Advance(30 * time.Minute) // less than 1h schedule[0]
	assert.False(t, s.Decide(peer).Allow,
		"post-success failure must reuse schedule[0], not advance")
}

func TestRecordFailure_ClassChangeResetsCounter(t *testing.T) {
	// A peer that was timing out and now responds with a KRPC error
	// is exhibiting a different failure mode. The new class's
	// schedule should start at index 0, not inherit the previous
	// class's run-length — otherwise a peer that briefly stops
	// responding then recovers to a new class state could get
	// hammered with the *long* end of the new schedule immediately.
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.DialTimeoutBackoff = []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	cfg.KRPCErrorBackoff = []time.Duration{1 * time.Hour, 6 * time.Hour}
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)
	peer := mkPeer("198.51.100.6:6881")

	// Two dial timeouts → consecutiveFailures=2 → would map to
	// DialTimeoutBackoff[1] = 30 min.
	s.RecordFailure(peer, ClassDialTimeout)
	s.RecordFailure(peer, ClassDialTimeout)

	// Class change to KRPC. New schedule must start at [0] = 1h, not
	// at [1] = 6h.
	s.RecordFailure(peer, ClassKRPCError)
	clk.Advance(50 * time.Minute) // less than 1h
	assert.False(t, s.Decide(peer).Allow, "fresh class must still be suppressing")
	clk.Advance(15 * time.Minute) // 65 min > 1h
	assert.True(t, s.Decide(peer).Allow,
		"first KRPC failure should only suppress for KRPCErrorBackoff[0]=1h")
}

// --- shadow mode --------------------------------------------------

func TestDecide_ShadowModeAllowsButFlags(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = false // shadow mode
	cfg.DialTimeoutBackoff = []time.Duration{1 * time.Hour}
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)
	peer := mkPeer("198.51.100.7:6881")

	s.RecordFailure(peer, ClassDialTimeout)

	// Within suppression window: shadow mode keeps Allow=true so
	// the fetcher still attempts, but WouldSkip=true so the metric
	// can count what enforce mode WOULD have skipped.
	d := s.Decide(peer)
	assert.True(t, d.Allow, "shadow mode must still allow attempts")
	assert.True(t, d.WouldSkip, "shadow mode must surface the skip-decision-as-counterfactual")
	assert.Equal(t, ClassDialTimeout, d.LastClass)
}

// --- ClassUnknown fallback ----------------------------------------

func TestRecordFailure_UnknownFallsBackToNetwork(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.NetworkErrorBackoff = []time.Duration{5 * time.Minute}
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)
	peer := mkPeer("198.51.100.8:6881")

	s.RecordFailure(peer, ClassUnknown)
	clk.Advance(2 * time.Minute)
	assert.False(t, s.Decide(peer).Allow,
		"ClassUnknown must use NetworkErrorBackoff schedule")
}

// --- LRU eviction ----------------------------------------------

func TestStore_LRUEvictsOldestPastCap(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.MaxEntries = 3
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)

	// Insert 3 + 1 — oldest must be evicted.
	for _, p := range []string{
		"198.51.100.10:6881",
		"198.51.100.11:6881",
		"198.51.100.12:6881",
	} {
		s.RecordFailure(mkPeer(p), ClassDialTimeout)
	}
	assert.Equal(t, 3, s.Len())
	s.RecordFailure(mkPeer("198.51.100.13:6881"), ClassDialTimeout)
	assert.Equal(t, 3, s.Len(), "MaxEntries must cap the LRU size")
}

func TestStore_ExpiryIsLazyAndResetsPeerHistory(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.TTL = time.Hour
	cfg.DialTimeoutBackoff = []time.Duration{5 * time.Minute, 6 * time.Hour}
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)
	peer := mkPeer("198.51.100.14:6881")

	// Two failures select the long second-stage backoff.
	s.RecordFailure(peer, ClassDialTimeout)
	s.RecordFailure(peer, ClassDialTimeout)
	clk.Advance(cfg.TTL + time.Nanosecond)

	// Access past TTL removes the stale entry. Expiry remains semantically
	// identical to the old expirable cache even though cleanup is now lazy.
	assert.True(t, s.Decide(peer).Allow)
	assert.Equal(t, 0, s.Len(), "expired entry must be removed when observed")

	// A new failure starts at schedule[0], rather than inheriting the stale
	// consecutive-failure count and its six-hour suppression.
	s.RecordFailure(peer, ClassDialTimeout)
	clk.Advance(6 * time.Minute)
	assert.True(t, s.Decide(peer).Allow,
		"post-expiry failure must restart from the first backoff step")
}

func TestNewStore_ZeroLimitsFallBackToBoundedDefaults(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.MaxEntries = 0
	cfg.TTL = 0

	s := NewStore(cfg, nil)
	for i := 0; i < defaultMaxEntries+5_000; i++ {
		s.upsert(netip.AddrPortFrom(
			netip.AddrFrom4([4]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}),
			uint16(i%65535),
		))
	}

	assert.Equal(t, defaultMaxEntries, s.Len(),
		"zero MaxEntries must fall back to a finite cap")
	assert.Equal(t, defaultTTL, s.cfg.TTL,
		"zero TTL must fall back to the shipping default")
}

// --- ErrorClass enumeration -----------------------------------

func TestErrorClass_StringStable(t *testing.T) {
	// Metric labels depend on these exact strings — pin them.
	assert.Equal(t, "unknown", ClassUnknown.String())
	assert.Equal(t, "dial_timeout", ClassDialTimeout.String())
	assert.Equal(t, "network_error", ClassNetworkError.String())
	assert.Equal(t, "handshake_fail", ClassHandshakeFail.String())
	assert.Equal(t, "no_ut_metadata", ClassNoUtMetadata.String())
	assert.Equal(t, "metadata_timeout", ClassMetadataTimeout.String())
	assert.Equal(t, "krpc_error", ClassKRPCError.String())
	// New classes added 2026-04-26 per gpt-5.5-pro senior review:
	assert.Equal(t, "connection_refused", ClassConnectionRefused.String())
	assert.Equal(t, "peer_closed", ClassPeerClosed.String())
	assert.Equal(t, "hash_mismatch", ClassHashMismatch.String())
}

// TestRecordFailure_NewClassesUseConfiguredSchedules pins the
// dispatch from the new ErrorClass values to their backoff configs.
// Forward-compat behaviour: if a deployed Config predates one of
// the new classes (zero-len schedule), the cache falls back to
// NetworkErrorBackoff rather than silently never suppressing.
func TestRecordFailure_NewClassesUseConfiguredSchedules(t *testing.T) {
	classes := []struct {
		name     string
		class    ErrorClass
		set      func(*Config)
		schedule time.Duration
	}{
		{
			"ConnectionRefused uses ConnectionRefusedBackoff",
			ClassConnectionRefused,
			func(c *Config) {
				c.ConnectionRefusedBackoff = []time.Duration{15 * time.Minute}
			},
			15 * time.Minute,
		},
		{
			"PeerClosed uses PeerClosedBackoff",
			ClassPeerClosed,
			func(c *Config) {
				c.PeerClosedBackoff = []time.Duration{20 * time.Minute}
			},
			20 * time.Minute,
		},
		{
			"HashMismatch uses HashMismatchBackoff",
			ClassHashMismatch,
			func(c *Config) {
				c.HashMismatchBackoff = []time.Duration{45 * time.Minute}
			},
			45 * time.Minute,
		},
	}
	for _, tt := range classes {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewDefaultConfig()
			cfg.Enabled = true
			cfg.Enforce = true
			tt.set(&cfg)
			clk := newFakeClock()
			s := NewStore(cfg, clk.Now)
			peer := mkPeer("198.51.100.42:6881")

			s.RecordFailure(peer, tt.class)
			// Just before the configured backoff — peer still suppressed.
			clk.Advance(tt.schedule - time.Second)
			assert.False(t, s.Decide(peer).Allow,
				"peer should be suppressed before backoff expires")
			// Just after — peer allowed again.
			clk.Advance(2 * time.Second)
			assert.True(t, s.Decide(peer).Allow,
				"peer should be allowed after backoff expires")
		})
	}
}

// TestRecordFailure_NewClassWithEmptyConfigFallsBack pins the
// forward-compat path: a deployed Config that predates one of the
// new classes (the corresponding *Backoff slice is nil) must not
// silently disable suppression — it should fall through to the
// NetworkErrorBackoff schedule.
func TestRecordFailure_NewClassWithEmptyConfigFallsBack(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.HashMismatchBackoff = nil // simulate older deployed config
	cfg.NetworkErrorBackoff = []time.Duration{8 * time.Minute}
	clk := newFakeClock()
	s := NewStore(cfg, clk.Now)
	peer := mkPeer("198.51.100.43:6881")

	s.RecordFailure(peer, ClassHashMismatch)
	clk.Advance(5 * time.Minute)
	assert.False(t, s.Decide(peer).Allow,
		"empty schedule must fall back to NetworkErrorBackoff (still suppressed at 5m)")
}

// --- defaults -------------------------------------------------

func TestNewDefaultConfig_SafeForShipping(t *testing.T) {
	cfg := NewDefaultConfig()
	assert.False(t, cfg.Enabled, "default must be off — explicit opt-in only")
	assert.False(t, cfg.Enforce, "Enforce default must be off — shadow first")
	assert.Greater(t, cfg.MaxEntries, 0)
	assert.Greater(t, cfg.TTL, time.Duration(0))
	for _, sched := range [][]time.Duration{
		cfg.DialTimeoutBackoff,
		cfg.NetworkErrorBackoff,
		cfg.HandshakeFailBackoff,
		cfg.NoUtMetadataBackoff,
		cfg.MetadataTimeoutBackoff,
		cfg.KRPCErrorBackoff,
	} {
		assert.NotEmpty(t, sched, "every error-class schedule must be non-empty")
	}
}

// A zero MaxEntries must NOT produce an unbounded cache. expirable.NewLRU
// treats size 0 as "no limit", so an unset or empty PEER_REP_MAX_ENTRIES
// would otherwise turn this into a map that grows until the TTL evicts —
// on a DHT crawler, millions of peers. Measured 2026-09-02: this LRU costs
// ~345 bytes/entry, so unbounded growth is ~345 MB per million peers.
func TestNewStore_ZeroMaxEntriesIsBoundedNotUnbounded(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.MaxEntries = 0
	cfg.TTL = 0

	s := NewStore(cfg, nil)
	for i := 0; i < defaultMaxEntries+5_000; i++ {
		s.upsert(netip.AddrPortFrom(
			netip.AddrFrom4([4]byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}),
			uint16(i%65535),
		))
	}

	assert.Equal(t, defaultMaxEntries, s.Len(),
		"zero MaxEntries must fall back to the default cap, not become unbounded")
	assert.Equal(t, defaultTTL, s.cfg.TTL, "zero TTL must fall back to the default")
}
