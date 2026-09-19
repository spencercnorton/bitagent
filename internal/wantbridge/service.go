package wantbridge

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
)

// Service is the live, *arr-polling Wantbridge implementation.
// Returned by the factory when Config.Enabled=true AND at least one
// *arr is configured. Otherwise the factory returns NoOp and this
// type stays out of memory entirely.
//
// Concurrency model:
//
//   - The poller goroutine writes to fingerprintTable + bloom under
//     mu (write lock).
//   - Match() readers acquire the read lock for a single map lookup.
//   - The bloom-filter library is internally goroutine-safe for
//     reads but needs external synchronisation for writes; we hold
//     the same mu.
//
// The poller swaps the entire table on each refresh — it's smaller
// than reasoning about per-key updates, and *arr wantlists are
// stable enough that whole-table rebuilds every 5min are cheap
// (~1k entries take <1ms to canonicalise + bloom-load).
type Service struct {
	cfg      Config
	clients  []arrClient
	logger   serviceLogger
	clock    func() time.Time
	pollOnce sync.Once

	// Hot-path read state. Protected by mu (RWMutex). Match()
	// takes the read lock; the poller takes the write lock at swap
	// time.
	mu               sync.RWMutex
	fingerprintTable map[string][]wantEntry // canonicalKey -> wantlist entries (1+)
	filter           *bloom.BloomFilter

	// Per-source state for Snapshot() + metrics.
	sourceState map[Source]*sourceState

	// Metrics callbacks. Optional; nil-safe.
	cb ServiceCallbacks
}

// ServiceCallbacks are lightweight hooks invoked from the service
// hot path. The wantbridgefx module wires concrete metrics
// implementations behind these so the wantbridge package itself
// stays free of Prometheus imports.
type ServiceCallbacks struct {
	OnMatch              func(tier Tier, source Source)
	OnPollOK             func(source Source, wantlistN int, duration time.Duration)
	OnPollErr            func(source Source, err error)
	OnFingerprintRebuild func(totalEntries int, duration time.Duration)
}

// serviceLogger lets tests pin log output. The fx wiring layer
// provides a *zap.SugaredLogger adapter.
type serviceLogger interface {
	Infow(msg string, keysAndValues ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
	Debugw(msg string, keysAndValues ...interface{})
}

type sourceState struct {
	wantlistN      int
	lastPollOK     time.Time
	lastPollErr    time.Time
	consecutiveErr int
}

// NewService builds the live wantbridge. Caller is responsible for
// having checked Config.Enabled + Config.HasAnySource() — this
// constructor doesn't second-guess and will happily return a
// service with zero clients (which then degrades to permanent
// "no match" Tier1 returns).
func NewService(cfg Config, logger serviceLogger, cb ServiceCallbacks) *Service {
	timeout := 30 * time.Second
	if d, err := time.ParseDuration(cfg.PollInterval); err == nil && d > 0 {
		// Per-call timeout: half the poll interval, up to 30s.
		timeout = d / 2
		if timeout > 30*time.Second {
			timeout = 30 * time.Second
		}
	}

	var clients []arrClient
	if cfg.SonarrBaseURL != "" && cfg.SonarrAPIKey != "" {
		clients = append(clients, newSonarrClient(cfg.SonarrBaseURL, cfg.SonarrAPIKey, timeout))
	}
	if cfg.RadarrBaseURL != "" && cfg.RadarrAPIKey != "" {
		clients = append(clients, newRadarrClient(cfg.RadarrBaseURL, cfg.RadarrAPIKey, timeout))
	}
	if cfg.LidarrBaseURL != "" && cfg.LidarrAPIKey != "" {
		clients = append(clients, newLidarrClient(cfg.LidarrBaseURL, cfg.LidarrAPIKey, timeout))
	}

	bloomCap := cfg.BloomCapacity
	if bloomCap <= 0 {
		bloomCap = 100_000
	}
	bloomFPR := cfg.BloomFalsePositiveRate
	if bloomFPR <= 0 || bloomFPR >= 1 {
		bloomFPR = 0.001
	}

	return &Service{
		cfg:              cfg,
		clients:          clients,
		logger:           logger,
		clock:            time.Now,
		fingerprintTable: make(map[string][]wantEntry),
		filter:           bloom.NewWithEstimates(uint(bloomCap), bloomFPR),
		sourceState:      make(map[Source]*sourceState),
		cb:               cb,
	}
}

// Enabled returns the operator's opt-in state.
func (s *Service) Enabled() bool { return s.cfg.Enabled }

// Enforce reports the live-mode flag. Match() always runs and emits
// metrics regardless; Enforce()=false tells the dhtcrawler queue to
// ignore the returned tier (counterfactual mode).
func (s *Service) Enforce() bool { return s.cfg.Enforce }

// Match is the hot-path call. Canonicalise the name, bloom-check,
// and on positive bloom hit do the exact lookup. Returns Tier0 with
// a Source on a real match, Tier1 otherwise.
//
// Implementation notes:
//   - Empty input or too-short tokens return Tier1 immediately
//     without acquiring the lock.
//   - Bloom miss is the common case (most DHT torrents are NOT
//     wanted) — short-circuits on a single bloom Lookup.
//   - Bloom hit but no exact match (false-positive) returns Tier1.
//     Counted in OnMatch as Tier1 (the bloom did its job: filtered
//     to a manageable check rate).
func (s *Service) Match(name string) MatchResult {
	if !s.cfg.Enabled {
		return MatchResult{Tier: Tier1}
	}
	c := Canonicalise(name)
	if len(c.Title) == 0 {
		// No usable title at all (all-noise input, or title
		// stripped to empty). Tier 1.
		s.fireMatch(Tier1, "")
		return MatchResult{Tier: Tier1}
	}
	// MinTitleTokens floor — set to 1 by default so single-word
	// titles like "Inception" / "Up" / "Her" are matchable. The
	// operator can raise it if false positives on generic 1-token
	// titles become a problem.
	if s.cfg.MinTitleTokens > 0 {
		if tokensIn(c.Title) < s.cfg.MinTitleTokens {
			s.fireMatch(Tier1, "")
			return MatchResult{Tier: Tier1}
		}
	}

	// Build the lookup keys. For Kind=Unknown (title only, no year
	// / season / music marker) we try ALL kind prefixes against the
	// bloom — the bloom + exact-match table will pick out a hit if
	// the operator's wantlist contains a matching title-only entry,
	// or short-circuit on the bloom if not.
	var keys []string
	if c.Kind == KindUnknown {
		keys = []string{
			"tv:" + c.Title,
			"mv:" + c.Title,
			"ms:" + c.Title,
		}
	} else {
		keys = canonicalKeys(c)
	}
	if len(keys) == 0 {
		s.fireMatch(Tier1, "")
		return MatchResult{Tier: Tier1}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, k := range keys {
		if !s.filter.TestString(k) {
			continue
		}
		entries, ok := s.fingerprintTable[k]
		if !ok || len(entries) == 0 {
			// Bloom false positive. Keep checking other keys.
			continue
		}
		// Pick the first entry as the canonical match. Multiple
		// *arrs can have overlapping wantlists (e.g. the operator
		// uses both Sonarr and Lidarr for a soundtrack series);
		// the first one wins for the Tier0 push, but the metric
		// hits all of them.
		entry := entries[0]
		s.fireMatch(Tier0, entry.Source)
		return MatchResult{
			Tier:      Tier0,
			Source:    entry.Source,
			Canonical: entry.Canonical,
		}
	}

	// Canonicalised but no wantlist match. Tier 1.
	s.fireMatch(Tier1, "")
	return MatchResult{Tier: Tier1}
}

// Snapshot returns a diagnostic view. Cheap; reads atomic gauges
// behind a read lock.
func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sources := make(map[Source]SourceSnapshot, len(s.sourceState))
	for src, st := range s.sourceState {
		sources[src] = SourceSnapshot{
			WantlistN:      st.wantlistN,
			LastPollOK:     st.lastPollOK,
			LastPollErr:    st.lastPollErr,
			ConsecutiveErr: st.consecutiveErr,
		}
	}
	var approxCount uint32
	if s.filter != nil {
		approxCount = s.filter.ApproximatedSize()
	}
	cap := uint(0)
	if s.filter != nil {
		cap = s.filter.Cap()
	}
	fillRate := 0.0
	if cap > 0 {
		fillRate = float64(approxCount) / float64(cap)
	}
	return Snapshot{
		Sources:       sources,
		FingerprintN:  len(s.fingerprintTable),
		BloomCapacity: int(cap),
		BloomFillRate: fillRate,
	}
}

// Refresh polls every configured *arr in parallel and rebuilds the
// canonical table on success. Safe to call from a periodic worker
// or manually from a debug endpoint.
//
// Failure semantics: a single *arr's failure does NOT clear that
// source's previous fingerprints. Stale-but-present is better than
// blank. Per-source error metric increments and the poller's log
// emits warn level. After ConsecutiveErr crosses a threshold (5
// consecutive failures) the source's fingerprints are evicted —
// at that point we have no fresh data and stale is worse than
// missing.
func (s *Service) Refresh(ctx context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	if len(s.clients) == 0 {
		return nil
	}

	// Fetch each source in parallel; collect into per-source
	// staging slots so a partial failure doesn't blank the table.
	type result struct {
		source Source
		got    []wantEntry
		err    error
		took   time.Duration
	}
	resCh := make(chan result, len(s.clients))
	var wg sync.WaitGroup
	for _, c := range s.clients {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := s.clock()
			got, err := c.Fetch(ctx)
			resCh <- result{
				source: c.Source(),
				got:    got,
				err:    err,
				took:   s.clock().Sub(t0),
			}
		}()
	}
	wg.Wait()
	close(resCh)

	// Aggregate. Build the new fingerprint table from the union of
	// successful sources + the previous state of failed sources
	// (within the consecutive-error tolerance).
	results := map[Source]result{}
	for r := range resCh {
		results[r.source] = r
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for src, r := range results {
		st := s.sourceState[src]
		if st == nil {
			st = &sourceState{}
			s.sourceState[src] = st
		}
		if r.err != nil {
			st.lastPollErr = s.clock()
			st.consecutiveErr++
			if s.cb.OnPollErr != nil {
				s.cb.OnPollErr(src, r.err)
			}
			if s.logger != nil {
				s.logger.Warnw("wantbridge: arr poll failed",
					"source", string(src), "err", r.err.Error(),
					"consecutive_errors", st.consecutiveErr)
			}
			continue
		}
		st.lastPollOK = s.clock()
		st.consecutiveErr = 0
		st.wantlistN = len(r.got)
		if s.cb.OnPollOK != nil {
			s.cb.OnPollOK(src, len(r.got), r.took)
		}
	}

	// Rebuild the fingerprint table + bloom from the union of
	// (fresh successful results) ∪ (previous-table entries from
	// sources within the error tolerance). The simplest implementation
	// is to rebuild from results only — sources that failed but are
	// within the tolerance keep their entries via "freshKeep" below.
	t0 := s.clock()
	newTable := make(map[string][]wantEntry)
	for src, r := range results {
		if r.err != nil {
			st := s.sourceState[src]
			if st != nil && st.consecutiveErr < 5 {
				// Within tolerance — keep previous entries by
				// re-injecting from the existing table where the
				// source matches. (We can't trivially keep them
				// in the new table without iterating; do that.)
				for k, entries := range s.fingerprintTable {
					for _, e := range entries {
						if e.Source != src {
							continue
						}
						newTable[k] = append(newTable[k], e)
					}
				}
			}
			continue
		}
		for _, entry := range r.got {
			for _, k := range canonicalKeys(entry.Canonical) {
				newTable[k] = append(newTable[k], entry)
			}
		}
	}

	// Allocate a fresh bloom; sized for the new total.
	bloomCap := s.cfg.BloomCapacity
	if bloomCap <= 0 {
		bloomCap = 100_000
	}
	bloomFPR := s.cfg.BloomFalsePositiveRate
	if bloomFPR <= 0 || bloomFPR >= 1 {
		bloomFPR = 0.001
	}
	newFilter := bloom.NewWithEstimates(uint(bloomCap), bloomFPR)
	for k := range newTable {
		newFilter.AddString(k)
	}

	s.fingerprintTable = newTable
	s.filter = newFilter

	if s.cb.OnFingerprintRebuild != nil {
		s.cb.OnFingerprintRebuild(len(newTable), s.clock().Sub(t0))
	}
	if s.logger != nil {
		s.logger.Infow("wantbridge: fingerprint table rebuilt",
			"keys", len(newTable),
			"bloom_capacity", bloomCap,
			"rebuild_ms", s.clock().Sub(t0).Milliseconds())
	}
	return nil
}

// Run starts the periodic poll loop. Call once from the worker
// runner. Returns when ctx is cancelled. Safe to call multiple
// times — only the first call starts the loop (sync.Once gate).
func (s *Service) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	s.pollOnce.Do(func() {
		go s.runLoop(ctx)
	})
}

func (s *Service) runLoop(ctx context.Context) {
	interval, err := time.ParseDuration(s.cfg.PollInterval)
	if err != nil || interval <= 0 {
		interval = 5 * time.Minute
	}
	// Fire immediately, then every interval.
	if err := s.Refresh(ctx); err != nil && s.logger != nil {
		s.logger.Warnw("wantbridge: initial refresh failed", "err", err.Error())
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ctx2, cancel := context.WithTimeout(ctx, interval)
			_ = s.Refresh(ctx2)
			cancel()
		}
	}
}

// fireMatch invokes the OnMatch callback safely.
func (s *Service) fireMatch(tier Tier, source Source) {
	if s.cb.OnMatch != nil {
		s.cb.OnMatch(tier, source)
	}
}

// tokensIn counts whitespace-separated tokens in a string. Used by
// the MinTitleTokens guard.
func tokensIn(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	inTok := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			inTok = false
			continue
		}
		if !inTok {
			n++
			inTok = true
		}
	}
	return n
}

// Compile-time assertion: Service satisfies Wantbridge.
var _ Wantbridge = (*Service)(nil)

// httpClientForTimeout is a small helper used by tests. Production
// uses each *arr client's own *http.Client (built in newSonarr/etc).
func httpClientForTimeout(d time.Duration) *http.Client {
	return &http.Client{Timeout: d}
}
