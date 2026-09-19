package csamblocklist

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/protocol"
)

func TestNoOp_AlwaysAllows(t *testing.T) {
	m := NewNoOp()
	var ih protocol.ID
	for i := range ih {
		ih[i] = byte(i)
	}
	if m.IsBlocked(ih) {
		t.Fatal("NoOp.IsBlocked must be false")
	}
	out := m.Filter([]protocol.ID{ih, ih, ih})
	if len(out) != 3 {
		t.Fatalf("NoOp.Filter dropped hashes: got %d, want 3", len(out))
	}
	if m.Enabled() {
		t.Fatal("NoOp.Enabled must be false")
	}
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatalf("NoOp.Refresh error: %v", err)
	}
}

func TestNew_NoOpWhenDisabled(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = false
	cfg.FeedUrls = []string{"https://example.invalid/feed"}
	m := New(cfg, nil, nil)
	if m.Enabled() {
		t.Fatal("New(disabled) returned enabled Manager")
	}
}

func TestNew_NoOpWhenNoFeed(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.FeedUrls = nil
	m := New(cfg, nil, nil)
	if m.Enabled() {
		t.Fatal("New(no feeds) returned enabled Manager")
	}
}

// makeFeed returns an httptest server that serves a feed body
// containing the given infohashes (as their double-hashes), one per line.
func makeFeed(t *testing.T, hashes ...protocol.ID) *httptest.Server {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("# test feed\n")
	for _, h := range hashes {
		sb.WriteString(DoubleHashHex(h))
		sb.WriteString("\n")
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(sb.String()))
	}))
}

func TestService_RefreshAndFilter(t *testing.T) {
	var bad protocol.ID
	for i := range bad {
		bad[i] = byte(0xab)
	}
	var good protocol.ID
	for i := range good {
		good[i] = byte(0xcd)
	}

	srv := makeFeed(t, bad)
	defer srv.Close()

	cfg := NewDefaultConfig()
	cfg.FeedUrls = []string{srv.URL}
	cfg.FeedRefreshInterval = time.Hour
	cfg.BloomCapacity = 1000
	mgr := NewService(cfg, nil, NewMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := mgr.Refresh(ctx); err != nil {
		t.Fatalf("Refresh error: %v", err)
	}
	if !mgr.IsBlocked(bad) {
		t.Fatal("expected bad infohash to be blocked")
	}
	if mgr.IsBlocked(good) {
		t.Fatal("good infohash unexpectedly blocked")
	}

	out := mgr.Filter([]protocol.ID{good, bad, good})
	if len(out) != 2 {
		t.Fatalf("Filter kept wrong count: got %d, want 2 (both 'good' should pass)", len(out))
	}
	for _, h := range out {
		if h == bad {
			t.Fatal("Filter let a blocked hash through")
		}
	}

	snap := mgr.Snapshot()
	if snap.EntryCount != 1 {
		t.Errorf("Snapshot.EntryCount = %d, want 1", snap.EntryCount)
	}
	if len(snap.PerFeed) != 1 {
		t.Errorf("Snapshot.PerFeed has %d entries, want 1", len(snap.PerFeed))
	}
	for _, fs := range snap.PerFeed {
		if fs.LastSuccessAt.IsZero() {
			t.Error("FeedStatus.LastSuccessAt is zero after successful refresh")
		}
		if fs.EntriesContributed != 1 {
			t.Errorf("FeedStatus.EntriesContributed = %d, want 1", fs.EntriesContributed)
		}
	}
}

func TestService_FeedFailureKeepsServiceUsable(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.FeedUrls = []string{"http://127.0.0.1:1/nonexistent"} // immediate connection refusal
	cfg.FeedRefreshInterval = time.Hour
	cfg.FeedFetchTimeout = 500 * time.Millisecond
	mgr := NewService(cfg, nil, NewMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := mgr.Refresh(ctx)
	if err == nil {
		t.Fatal("expected non-nil error from failed feed")
	}

	var ih protocol.ID
	if mgr.IsBlocked(ih) {
		t.Fatal("expected service with failed feed to allow all hashes")
	}
}

func TestParseFeed_AcceptsCommentsAndBlanks(t *testing.T) {
	body := `
# comment
# another comment

de47c9b27eb8d300dbb5f2c353e632c393262cf06340c4fa7f1b40c4cbd36f90
`
	entries, parseErrs, err := parseFeed(strings.NewReader(body), 1024)
	if err != nil {
		t.Fatalf("parseFeed error: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1", len(entries))
	}
	if parseErrs != 0 {
		t.Errorf("got %d parse errors, want 0", parseErrs)
	}
}

func TestParseFeed_DedupesEntries(t *testing.T) {
	body := strings.Repeat("de47c9b27eb8d300dbb5f2c353e632c393262cf06340c4fa7f1b40c4cbd36f90\n", 5)
	entries, parseErrs, err := parseFeed(strings.NewReader(body), 1024)
	if err != nil {
		t.Fatalf("parseFeed error: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("dedupe failed: got %d entries, want 1", len(entries))
	}
	if parseErrs != 0 {
		t.Errorf("got %d parse errors, want 0", parseErrs)
	}
}

func TestParseFeed_CountsParseErrorsButContinues(t *testing.T) {
	body := `de47c9b27eb8d300dbb5f2c353e632c393262cf06340c4fa7f1b40c4cbd36f90
this-is-not-a-hex-line
NOTHEX
` + DoubleHashHex(protocol.ID{1}) + "\n"
	entries, parseErrs, err := parseFeed(strings.NewReader(body), 4096)
	if err != nil {
		t.Fatalf("parseFeed error: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d valid entries, want 2", len(entries))
	}
	if parseErrs != 2 {
		t.Errorf("got %d parse errors, want 2", parseErrs)
	}
}

func TestParseFeed_RejectsExcessiveSize(t *testing.T) {
	hexLine := DoubleHashHex(protocol.ID{1}) + "\n"
	body := strings.Repeat(hexLine, 10)
	_, _, err := parseFeed(strings.NewReader(body), int64(len(hexLine)*3))
	if err == nil {
		t.Fatal("expected size-limit error")
	}
}

func TestFeedLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://example.com/feed.txt", "example.com/feed.txt"},
		{"http://example.com/path/to?ignored=1", "example.com/path/to"},
		{"not a url", "invalid"},
		{"https://", "invalid"},
	}
	for _, c := range cases {
		got := feedLabel(c.in)
		if got != c.want {
			t.Errorf("feedLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Per-feed last-good cache: a feed that fails AFTER a previous
// successful refresh must retain its previously-loaded entries —
// the active bloom should still block those infohashes after the
// failure (review finding: "Feed refresh failures drop previously
// loaded blocklist entries").
func TestService_FeedFailureRetainsPreviouslyLoadedEntries(t *testing.T) {
	var bad protocol.ID
	for i := range bad {
		bad[i] = byte(0xab)
	}

	// Feed serves the entry once, then 500s. Toggled by an atomic
	// counter so only the first request sees a 200.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomicAdd(&calls, 1)
		if n == 1 {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(DoubleHashHex(bad) + "\n"))
			return
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := NewDefaultConfig()
	cfg.FeedUrls = []string{srv.URL}
	cfg.FeedRefreshInterval = time.Hour
	mgr := NewService(cfg, nil, NewMetrics())

	// First refresh: success, entry loaded.
	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh #1 error: %v", err)
	}
	if !mgr.IsBlocked(bad) {
		t.Fatal("after Refresh #1, expected bad infohash to be blocked")
	}
	snap := mgr.Snapshot()
	if snap.EntryCount != 1 {
		t.Fatalf("after Refresh #1, EntryCount = %d, want 1", snap.EntryCount)
	}

	// Second refresh: feed returns 500.
	err := mgr.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh #2 expected to return an error")
	}

	// CRITICAL: even though the feed failed, the previously-loaded
	// entry MUST still block.
	if !mgr.IsBlocked(bad) {
		t.Fatal("after failed Refresh #2, expected bad infohash to STILL be blocked (per-feed last-good cache)")
	}
	snap2 := mgr.Snapshot()
	if snap2.EntryCount != 1 {
		t.Errorf("after failed Refresh #2, EntryCount = %d, want 1 (cache retained)", snap2.EntryCount)
	}
	for _, fs := range snap2.PerFeed {
		if fs.LastError == "" {
			t.Error("expected LastError to be populated after failure")
		}
		if fs.EntriesContributed != 1 {
			t.Errorf("EntriesContributed = %d, want 1 (cached count, not zero)", fs.EntriesContributed)
		}
	}
}

// Multi-feed: when one feed succeeds and another fails on the same
// refresh, the union bloom contains both — the failing feed's
// previously-cached entries (if any) plus the succeeding feed's
// fresh entries.
func TestService_MixedFeedOutcomesUnionBloom(t *testing.T) {
	var hashA, hashB protocol.ID
	for i := range hashA {
		hashA[i] = byte(0x11)
		hashB[i] = byte(0x22)
	}

	feedA := makeFeed(t, hashA)
	defer feedA.Close()

	// feedB always 500s — never has a cache entry.
	feedB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer feedB.Close()

	cfg := NewDefaultConfig()
	cfg.FeedUrls = []string{feedA.URL, feedB.URL}
	cfg.FeedRefreshInterval = time.Hour
	mgr := NewService(cfg, nil, NewMetrics())

	err := mgr.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected per-feed error from feedB")
	}
	if !mgr.IsBlocked(hashA) {
		t.Error("hashA from succeeding feed should be blocked")
	}
	if mgr.IsBlocked(hashB) {
		t.Error("hashB from failing feed should NOT be blocked (never had a successful refresh)")
	}
}

func atomicAdd(p *int32, delta int32) int32 {
	return atomic.AddInt32(p, delta)
}

// Sanity: the bloom filter's FPR isn't catastrophic at low fill.
func TestService_LowFalsePositiveAtSparseFill(t *testing.T) {
	var bad protocol.ID
	for i := range bad {
		bad[i] = byte(i*17 + 3)
	}
	srv := makeFeed(t, bad)
	defer srv.Close()

	cfg := NewDefaultConfig()
	cfg.FeedUrls = []string{srv.URL}
	cfg.BloomCapacity = 1000
	cfg.BloomFalsePositiveRate = 0.001
	mgr := NewService(cfg, nil, NewMetrics())

	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh error: %v", err)
	}

	const probes = 1000
	fps := 0
	for i := range probes {
		var probe protocol.ID
		// Vary the bytes uniformly so probes don't accidentally
		// match `bad`.
		for j := range probe {
			probe[j] = byte((i*j + 1) ^ 0xa5)
		}
		if probe == bad {
			continue
		}
		if mgr.IsBlocked(probe) {
			fps++
		}
	}
	// 1000 probes against a 1-entry bloom with FPR=0.001 should
	// produce ~1 FP. Allow up to 10 (10x tolerance is generous).
	if fps > 10 {
		t.Errorf("too many false positives: %d / %d (target ~1)", fps, probes)
	}
	_ = fmt.Sprintf("") // silence unused import if test trimmed
}
