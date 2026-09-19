package externalip

import (
	"context"
	"errors"
	mrand "math/rand/v2"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRand returns a deterministic PRNG for jitter tests.
func testRand(t *testing.T) *mrand.Rand {
	t.Helper()
	return mrand.New(mrand.NewPCG(1, 2))
}

// scriptedResolver returns a queue of results in order. Once the
// queue is exhausted it keeps returning the last result. Thread-safe.
type scriptedResolver struct {
	mu      sync.Mutex
	results []resolverResult
	calls   atomic.Int32
}

type resolverResult struct {
	addr netip.Addr
	err  error
}

func (s *scriptedResolver) Resolve(_ context.Context) (netip.Addr, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()

	var r resolverResult
	if len(s.results) == 0 {
		return netip.Addr{}, errors.New("scriptedResolver: no result")
	}

	if len(s.results) == 1 {
		r = s.results[0]
	} else {
		r = s.results[0]
		s.results = s.results[1:]
	}

	return r.addr, r.err
}

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}

	return a
}

// fastWatcherCfg is the tight-loop variant used by tests. Jitter is
// disabled for determinism.
func fastWatcherCfg(interval time.Duration, confirmations int) WatcherConfig {
	return WatcherConfig{
		Interval:                 interval,
		JitterFraction:           -1, // explicit disable
		PerCheckTimeout:          100 * time.Millisecond,
		ConsecutiveConfirmations: confirmations,
	}
}

// secureIDFor produces a valid BEP-42 node ID for the given IP, so
// tests can assert that the verify-based trigger does the right thing
// with IDs that are actually compliant.
func secureIDFor(t *testing.T, ip netip.Addr) protocol.ID {
	t.Helper()
	id, err := protocol.SecureNodeID(ip)
	require.NoError(t, err)
	require.True(t, protocol.VerifySecureNodeID(id, ip))

	return id
}

func TestWatcher_invalidatingChangeFiresAfterConfirmations(t *testing.T) {
	t.Parallel()

	boot := addr(t, "198.51.100.1")
	nextIP := addr(t, "198.51.100.2")
	bootID := secureIDFor(t, boot)

	// BootID does NOT verify for nextIP (different CRC32C prefix),
	// so each successive resolve of nextIP is "invalidating". Needs
	// 2 consecutive invalidating resolves before firing.
	require.False(t, protocol.VerifySecureNodeID(bootID, nextIP))

	r := &scriptedResolver{results: []resolverResult{{addr: nextIP}}}

	fired := atomic.Int32{}
	var sawOld, sawNew netip.Addr
	onChange := func(old, current netip.Addr) {
		fired.Add(1)
		sawOld, sawNew = old, current
	}

	w := NewWatcher(r, bootID, boot, false,
		fastWatcherCfg(5*time.Millisecond, 2),
		onChange, testLogger(t), NewWatcherMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	w.Run(ctx)

	assert.Equal(t, int32(1), fired.Load(), "onChange must fire exactly once")
	assert.Equal(t, boot, sawOld)
	assert.Equal(t, nextIP, sawNew)
	assert.GreaterOrEqual(t, r.calls.Load(), int32(2),
		"needed at least 2 resolves to satisfy confirmation count")
}

func TestWatcher_matchingNodeIDDoesNotFire(t *testing.T) {
	t.Parallel()

	boot := addr(t, "198.51.100.3")
	bootID := secureIDFor(t, boot)

	// Resolver always returns the same valid-for-bootID IP. Verify
	// still passes → no fire.
	r := &scriptedResolver{results: []resolverResult{{addr: boot}}}

	fired := atomic.Int32{}
	w := NewWatcher(r, bootID, boot, false,
		fastWatcherCfg(5*time.Millisecond, 2),
		func(netip.Addr, netip.Addr) { fired.Add(1) },
		testLogger(t), NewWatcherMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	w.Run(ctx)

	assert.GreaterOrEqual(t, r.calls.Load(), int32(3))
	assert.Equal(t, int32(0), fired.Load(),
		"resolved IP that still verifies against bootID must not fire")
}

func TestWatcher_resolverErrorDoesNotFire(t *testing.T) {
	t.Parallel()

	boot := addr(t, "198.51.100.4")
	bootID := secureIDFor(t, boot)

	r := &scriptedResolver{results: []resolverResult{{err: errors.New("boom")}}}

	fired := atomic.Int32{}
	w := NewWatcher(r, bootID, boot, false,
		fastWatcherCfg(5*time.Millisecond, 2),
		func(netip.Addr, netip.Addr) { fired.Add(1) },
		testLogger(t), NewWatcherMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	w.Run(ctx)

	assert.Equal(t, int32(0), fired.Load(),
		"transient resolver errors must not bounce the container")
}

func TestWatcher_singleTransientFlipDoesNotFire(t *testing.T) {
	t.Parallel()

	boot := addr(t, "198.51.100.5")
	flip := addr(t, "198.51.100.6")
	bootID := secureIDFor(t, boot)

	// Sequence: one flip (invalidating), then resolver returns the
	// original IP again. The flip increments pendingConfirmations to
	// 1 but the next resolve resets it because it verifies. No fire.
	r := &scriptedResolver{results: []resolverResult{
		{addr: flip},
		{addr: boot},
	}}

	fired := atomic.Int32{}
	w := NewWatcher(r, bootID, boot, false,
		fastWatcherCfg(5*time.Millisecond, 2),
		func(netip.Addr, netip.Addr) { fired.Add(1) },
		testLogger(t), NewWatcherMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	w.Run(ctx)

	assert.Equal(t, int32(0), fired.Load(),
		"a single transient invalidating flip must not trigger restart "+
			"when confirmation count is 2")
}

func TestWatcher_randomFallbackFiresOnFirstConfirmedValidResolve(t *testing.T) {
	t.Parallel()

	// Boot fell back to random — bootID is random junk, bootIP is zero.
	// Startup self-heal: any confirmed valid resolve graduates us.
	bootID := protocol.RandomNodeIDWithClientSuffix()
	next := addr(t, "198.51.100.7")

	r := &scriptedResolver{results: []resolverResult{{addr: next}}}

	fired := atomic.Int32{}
	var sawOld, sawNew netip.Addr
	onChange := func(old, current netip.Addr) {
		fired.Add(1)
		sawOld, sawNew = old, current
	}

	w := NewWatcher(r, bootID, netip.Addr{}, true,
		fastWatcherCfg(5*time.Millisecond, 2),
		onChange, testLogger(t), NewWatcherMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	w.Run(ctx)

	assert.Equal(t, int32(1), fired.Load(), "random-fallback must graduate on confirmed valid resolve")
	assert.False(t, sawOld.IsValid(), "old IP is zero when we boot in fallback")
	assert.Equal(t, next, sawNew)
}

func TestWatcher_ctxDoneStopsCleanly(t *testing.T) {
	t.Parallel()

	boot := addr(t, "198.51.100.8")
	bootID := secureIDFor(t, boot)

	r := &scriptedResolver{results: []resolverResult{{addr: boot}}}

	fired := atomic.Int32{}
	w := NewWatcher(r, bootID, boot, false,
		fastWatcherCfg(50*time.Millisecond, 2),
		func(netip.Addr, netip.Addr) { fired.Add(1) },
		testLogger(t), NewWatcherMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestWatcher_jitterWithinBounds(t *testing.T) {
	t.Parallel()

	w := &Watcher{
		cfg: WatcherConfig{
			Interval:       100 * time.Millisecond,
			JitterFraction: 0.1,
		},
	}
	// Deterministic rand for the test:
	w.rand = testRand(t)

	for i := 0; i < 100; i++ {
		d := w.nextSleep()
		assert.GreaterOrEqual(t, d, 90*time.Millisecond)
		assert.LessOrEqual(t, d, 110*time.Millisecond)
	}
}

func TestWatcher_jitterDisabledReturnsExactInterval(t *testing.T) {
	t.Parallel()

	w := &Watcher{
		cfg: WatcherConfig{
			Interval:       100 * time.Millisecond,
			JitterFraction: 0,
		},
	}

	// With JitterFraction=0, nextSleep should return the exact
	// interval regardless of rand source.
	assert.Equal(t, 100*time.Millisecond, w.nextSleep())
}
