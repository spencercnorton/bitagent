package csamblocklistfx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"go.uber.org/zap"
)

// runStartupRefresh — when called with a populated feed, the active
// bloom contains the configured feed's entries by the time the call
// returns (review fix: "Initial blocklist refresh does not block
// crawler startup").
func TestRunStartupRefresh_BlocksUntilFirstFetchCompletes(t *testing.T) {
	var bad protocol.ID
	for i := range bad {
		bad[i] = byte(0x55)
	}

	// Feed waits 200ms before serving — gives a clear gap to verify
	// the helper actually waits.
	var fetched atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(csamblocklist.DoubleHashHex(bad) + "\n"))
		fetched.Add(1)
	}))
	defer srv.Close()

	cfg := csamblocklist.NewDefaultConfig()
	cfg.FeedUrls = []string{srv.URL}
	cfg.FeedFetchTimeout = 5 * time.Second
	logger := zap.NewNop().Sugar()
	mgr := csamblocklist.NewService(cfg, logger, csamblocklist.NewMetrics())

	startTime := time.Now()
	runStartupRefresh(context.Background(), mgr, logger, 5*time.Second)
	elapsed := time.Since(startTime)

	if elapsed < 150*time.Millisecond {
		t.Errorf("runStartupRefresh returned in %v — expected to block on initial refresh (>= 200ms)", elapsed)
	}
	if got := fetched.Load(); got < 1 {
		t.Errorf("feed not fetched: count=%d", got)
	}
	if !mgr.IsBlocked(bad) {
		t.Fatal("after runStartupRefresh, expected bad infohash to be blocked")
	}
}

// runStartupRefresh — when the feed hangs, the call returns within
// the configured budget rather than blocking forever. The active
// bloom remains empty, but the app is allowed to boot (fail-open).
//
// The handler honours the request's own context so that when the
// outer fetch ctx times out (FeedFetchTimeout shorter than the
// budget), the handler exits cleanly. This avoids a deferred
// httptest.Server.Close() blocking on an in-flight handler.
func TestRunStartupRefresh_BoundedByBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := csamblocklist.NewDefaultConfig()
	cfg.FeedUrls = []string{srv.URL}
	// Per-feed timeout shorter than the outer budget so the fetch
	// gives up cleanly before the OnStart budget elapses, AND the
	// handler's request context cancels cleanly when the client
	// hangs up.
	cfg.FeedFetchTimeout = 200 * time.Millisecond
	logger := zap.NewNop().Sugar()
	mgr := csamblocklist.NewService(cfg, logger, csamblocklist.NewMetrics())

	startTime := time.Now()
	runStartupRefresh(context.Background(), mgr, logger, 2*time.Second)
	elapsed := time.Since(startTime)

	// Should give up well within the outer budget — the per-feed
	// fetch timeout fires first.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("returned too late: %v > 1.5s (budget=2s, fetch_timeout=200ms)", elapsed)
	}

	// Active bloom is empty (no feed succeeded).
	var ih protocol.ID
	if mgr.IsBlocked(ih) {
		t.Error("expected empty bloom after failed initial refresh")
	}
}

// runStartupRefresh — when manager is NoOp (Enabled=false), call
// returns immediately without doing any work.
func TestRunStartupRefresh_NoOpReturnsImmediately(t *testing.T) {
	cfg := csamblocklist.NewDefaultConfig()
	cfg.Enabled = false
	logger := zap.NewNop().Sugar()
	mgr := csamblocklist.New(cfg, logger, csamblocklist.NewMetrics())

	startTime := time.Now()
	runStartupRefresh(context.Background(), mgr, logger, 5*time.Second)
	elapsed := time.Since(startTime)

	if elapsed > 50*time.Millisecond {
		t.Errorf("NoOp.runStartupRefresh took %v — expected <50ms", elapsed)
	}
}
