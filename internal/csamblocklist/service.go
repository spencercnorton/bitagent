package csamblocklist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"go.uber.org/zap"
)

// service is the live Manager implementation. Exposed via the Manager
// interface; constructed by the factory in csamblocklistfx.
//
// Concurrency model:
//   - active is an atomic pointer to the current bloom filter; reads
//     (IsBlocked / Filter) are lock-free.
//   - Refresh builds a fresh bloom filter from every configured feed
//     in parallel, then atomically swaps active. No reader sees a
//     partially-built filter.
//   - perFeed status map is protected by mutex; touched only by
//     Refresh and by Snapshot diagnostics.
type service struct {
	cfg     Config
	metrics *Metrics
	logger  *zap.SugaredLogger
	client  *http.Client

	active atomic.Pointer[bloom.BloomFilter]

	mu      sync.Mutex
	perFeed map[string]FeedStatus
	// lastGoodEntries caches the most recent SUCCESSFUL fetch result
	// per feed URL. On Refresh, feeds that succeed update their cache
	// entry; feeds that fail retain their previous cache. The union
	// bloom is rebuilt from the cache for ALL feeds, so a transient
	// feed outage does not drop previously loaded entries (Jeeves
	// review finding: "Feed refresh failures drop previously loaded
	// blocklist entries"). A feed that has NEVER succeeded simply
	// has no cache entry and contributes nothing.
	lastGoodEntries map[string][][DoubleHashLen]byte
	lastRefreshAt   time.Time
	entryCount      atomic.Int64
}

// NewService constructs the live Manager. The bloom filter starts
// empty (every IsBlocked returns false until the first Refresh
// completes); the worker triggers Refresh asynchronously on startup.
func NewService(cfg Config, logger *zap.SugaredLogger, metrics *Metrics) Manager {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	s := &service{
		cfg:     cfg,
		metrics: metrics,
		logger:  logger,
		client: &http.Client{
			Timeout: cfg.FeedFetchTimeout,
		},
		perFeed:         make(map[string]FeedStatus, len(cfg.FeedUrls)),
		lastGoodEntries: make(map[string][][DoubleHashLen]byte, len(cfg.FeedUrls)),
	}
	// Empty bloom so reads before first refresh are O(1) and return
	// false for every hash. Sized to operator's configured capacity
	// so post-Refresh swaps don't change allocation behaviour.
	empty := bloom.NewWithEstimates(maxUint(cfg.BloomCapacity, 1), cfg.BloomFalsePositiveRate)
	s.active.Store(empty)
	for _, u := range cfg.FeedUrls {
		if u == "" {
			continue
		}
		s.perFeed[u] = FeedStatus{URL: u}
	}
	return s
}

// New is the factory used by csamblocklistfx. Returns NoOp when the
// operator hasn't enabled the feature or hasn't configured any feed.
func New(cfg Config, logger *zap.SugaredLogger, metrics *Metrics) Manager {
	if !cfg.Enabled {
		return NewNoOp()
	}
	if !cfg.HasAnyFeed() {
		// Degenerate live mode: feed list is empty, so every
		// IsBlocked check tests an empty filter and returns false.
		// Returning the live service here (not NoOp) means the
		// operator can add a feed via env + restart and the wiring
		// is already in place. But it does cost one extra atomic
		// load + bloom test per discovery, so prefer NoOp until
		// we have signal that the wiring matters.
		return NewNoOp()
	}
	return NewService(cfg, logger, metrics)
}

// IsBlocked is the hot-path call. Atomic load + bloom test, no locks.
func (s *service) IsBlocked(infoHash protocol.ID) bool {
	if s.metrics != nil {
		s.metrics.LookupsTotal.Inc()
	}
	bf := s.active.Load()
	if bf == nil {
		return false
	}
	dh := DoubleHash(infoHash)
	return bf.Test(dh[:])
}

// Filter returns the unblocked subset, mirroring BlockingManager.Filter
// signature so call sites read consistently.
func (s *service) Filter(infoHashes []protocol.ID) []protocol.ID {
	bf := s.active.Load()
	if bf == nil {
		return infoHashes
	}
	out := infoHashes[:0:len(infoHashes)]
	for _, h := range infoHashes {
		if s.metrics != nil {
			s.metrics.LookupsTotal.Inc()
		}
		dh := DoubleHash(h)
		if bf.Test(dh[:]) {
			if s.metrics != nil {
				s.metrics.PrefetchBlocksTotal.Inc()
			}
			continue
		}
		out = append(out, h)
	}
	return out
}

// Refresh polls every configured feed in parallel, builds a single
// new bloom filter from the union of all per-feed last-good caches,
// and atomically swaps it in.
//
// Per-feed errors are logged + counted but do not abort the refresh —
// feeds that succeed update their cache; feeds that fail retain their
// previous cache (if any). A feed that has never succeeded simply
// contributes nothing. The active bloom is rebuilt every refresh
// from the union of the cache, so the bloom filter never shrinks due
// to a transient feed outage.
//
// Returns the first feed error seen, or nil. Caller's worker loop
// logs but does not bail on this error — the next interval retries.
func (s *service) Refresh(ctx context.Context) error {
	feeds := make([]string, 0, len(s.cfg.FeedUrls))
	for _, u := range s.cfg.FeedUrls {
		if u != "" {
			feeds = append(feeds, u)
		}
	}
	if len(feeds) == 0 {
		// Idempotent — clear filter + cache and report success.
		empty := bloom.NewWithEstimates(maxUint(s.cfg.BloomCapacity, 1), s.cfg.BloomFalsePositiveRate)
		s.active.Store(empty)
		s.entryCount.Store(0)
		s.mu.Lock()
		s.lastGoodEntries = make(map[string][][DoubleHashLen]byte)
		s.lastRefreshAt = time.Now()
		s.mu.Unlock()
		return nil
	}

	type feedResult struct {
		url     string
		entries [][DoubleHashLen]byte
		err     error
		took    time.Duration
	}
	results := make([]feedResult, len(feeds))
	var wg sync.WaitGroup
	for i, u := range feeds {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			t0 := time.Now()
			entries, err := s.fetchFeed(ctx, u)
			results[i] = feedResult{
				url:     u,
				entries: entries,
				err:     err,
				took:    time.Since(t0),
			}
		}(i, u)
	}
	wg.Wait()

	var firstErr error

	s.mu.Lock()
	for _, r := range results {
		st := s.perFeed[r.url]
		st.URL = r.url
		now := time.Now()
		if r.err != nil {
			st.LastError = r.err.Error()
			st.LastErrorAt = now
			st.ConsecutiveErrs++
			if firstErr == nil {
				firstErr = fmt.Errorf("feed %s: %w", r.url, r.err)
			}
			if s.metrics != nil {
				s.metrics.FeedRefreshTotal.WithLabelValues(feedLabel(r.url), "error").Inc()
				s.metrics.FeedRefreshDurationSeconds.WithLabelValues(feedLabel(r.url)).Observe(r.took.Seconds())
			}
			cachedN := len(s.lastGoodEntries[r.url])
			s.logger.Warnw("csam blocklist feed refresh failed; retaining cached entries",
				"url", r.url, "err", r.err, "consecutive_errs", st.ConsecutiveErrs, "retained_entries", cachedN)
			// IMPORTANT: do NOT touch s.lastGoodEntries[r.url] — keep
			// the previous-good cache so the union bloom retains those
			// entries when rebuilt below. EntriesContributed reflects
			// the cached count, not zero, so the operator dashboard
			// shows the real protection coverage even during outages.
			st.EntriesContributed = cachedN
		} else {
			st.LastSuccessAt = now
			st.ConsecutiveErrs = 0
			st.LastError = ""
			st.EntriesContributed = len(r.entries)
			s.lastGoodEntries[r.url] = r.entries
			if s.metrics != nil {
				s.metrics.FeedRefreshTotal.WithLabelValues(feedLabel(r.url), "success").Inc()
				s.metrics.FeedRefreshDurationSeconds.WithLabelValues(feedLabel(r.url)).Observe(r.took.Seconds())
				s.metrics.FeedEntriesGauge.WithLabelValues(feedLabel(r.url)).Set(float64(len(r.entries)))
			}
		}
		s.perFeed[r.url] = st
	}
	s.lastRefreshAt = time.Now()

	// Build the union bloom from EVERY feed's last-good cache (whether
	// the most recent attempt succeeded or failed). This is the core of
	// fix #2: failed feeds keep contributing their previously-loaded
	// entries until the next successful refresh replaces the cache.
	newBloom := bloom.NewWithEstimates(maxUint(s.cfg.BloomCapacity, 1), s.cfg.BloomFalsePositiveRate)
	totalEntries := 0
	for _, entries := range s.lastGoodEntries {
		for _, e := range entries {
			newBloom.Add(e[:])
		}
		totalEntries += len(entries)
	}
	s.mu.Unlock()

	s.active.Store(newBloom)
	s.entryCount.Store(int64(totalEntries))
	if s.metrics != nil {
		s.metrics.EntriesGauge.Set(float64(totalEntries))
	}

	return firstErr
}

func (s *service) Enabled() bool { return true }

func (s *service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	pf := make(map[string]FeedStatus, len(s.perFeed))
	for k, v := range s.perFeed {
		pf[k] = v
	}
	return Snapshot{
		EntryCount:             int(s.entryCount.Load()),
		BloomCapacity:          int(s.cfg.BloomCapacity),
		BloomFalsePositiveRate: s.cfg.BloomFalsePositiveRate,
		LastRefreshAt:          s.lastRefreshAt,
		PerFeed:                pf,
	}
}

// Run is the periodic poll loop. Called from the fx worker hook;
// returns when ctx is cancelled.
//
// The fx OnStart hook (csamblocklistfx.provideRefreshWorker) does the
// FIRST refresh synchronously before launching this loop, so the
// dhtcrawler never sees an empty filter when traffic starts flowing.
// This loop only services the periodic refresh interval — it does
// NOT do an initial refresh on entry.
func (s *service) Run(ctx context.Context) {
	tick := s.cfg.FeedRefreshInterval
	if tick <= 0 {
		tick = 6 * time.Hour
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Refresh(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					s.logger.Warnw("csam blocklist periodic refresh had errors", "err", err)
				}
			}
		}
	}
}

func maxUint(a uint, b uint) uint {
	if a > b {
		return a
	}
	return b
}
