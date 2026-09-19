package priors

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/evidence"
)

// rankerStore is the subset of *Store the ranker reads. Lets tests
// drive ranking with an in-memory fake.
type rankerStore interface {
	LookupPriors(ctx context.Context, keys []FeatureKey) (map[FeatureKey]Prior, error)
	GlobalAverage(ctx context.Context) (float64, error)
}

// Ranker is the Torznab adapter's hook into the priors module.
// Implements the adapter.Reranker interface (defined alongside the
// adapter). One instance per process; methods are safe for
// concurrent callers because LookupPriors / GlobalAverage are pure
// reads and the global-average cache is guarded by avgMu.
type Ranker struct {
	store    rankerStore
	cfg      evidence.OutcomePriorsConfig
	metrics  *Metrics
	now      func() time.Time
	cacheTTL time.Duration

	avgMu      sync.RWMutex
	avgCache   float64
	avgFetched time.Time
}

// NewRanker constructs a Ranker. The global-average cache TTL is set
// internally — the value changes slowly (it's a sum of two integers
// across the whole table) and re-fetching it on every search would
// waste a round trip.
func NewRanker(s *Store, cfg evidence.OutcomePriorsConfig, metrics *Metrics) *Ranker {
	return &Ranker{
		store:    s,
		cfg:      cfg,
		metrics:  metrics,
		now:      time.Now,
		cacheTTL: 5 * time.Minute,
		avgCache: 0.5,
	}
}

// Rerank reorders items in-place by descending posterior score.
// Returns the same slice (possibly with shuffled element order) so
// callers can keep their existing wrapping shape without extra
// allocations.
//
// When cfg.Enabled is false this is a no-op. When cfg.Apply is false
// the score is computed (so metrics and shadow-mode dashboards are
// populated) but the original order is preserved — operators can
// validate the ranker's effect before trusting it.
func (r *Ranker) Rerank(ctx context.Context, items []search.TorrentContentResultItem) []search.TorrentContentResultItem {
	if !r.cfg.Enabled || len(items) <= 1 {
		return items
	}
	scored := r.score(ctx, items)
	if scored == nil {
		return items
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// Displacement is recorded in BOTH modes. Shadow mode previously counted
	// only how many items it scored, which meant it could not answer the one
	// question it exists to answer — what would change if Apply were true.
	// Without that, there is never evidence to justify flipping the flag, so
	// the module scores every search forever and the result is discarded.
	// The sort above is over a local slice of (index, score) pairs; the
	// caller's ordering is only touched below, under Apply.
	for newIdx, s := range scored {
		r.metrics.RerankShift(newIdx - s.origIdx)
	}
	r.metrics.RerankBatch(len(items))

	if !r.cfg.Apply {
		return items
	}

	out := make([]search.TorrentContentResultItem, len(scored))
	for newIdx, s := range scored {
		out[newIdx] = items[s.origIdx]
	}
	copy(items, out)
	return items
}

type scoredItem struct {
	origIdx int
	score   float64
}

// score computes a posterior score for every item. Returns nil if
// the prior lookup fails — the adapter falls back to original order
// in that case (availability of search > rank quality).
func (r *Ranker) score(ctx context.Context, items []search.TorrentContentResultItem) []scoredItem {
	allKeys := make([]FeatureKey, 0, len(items)*5)
	perItem := make([][]FeatureKey, len(items))
	for i, item := range items {
		title := item.Torrent.Name
		ext := ""
		if item.Torrent.Extension.Valid {
			ext = item.Torrent.Extension.String
		}
		sources := make([]string, 0, len(item.Torrent.Sources))
		for _, src := range item.Torrent.Sources {
			sources = append(sources, src.Source)
		}
		ks := Extract(title, sources, ext)
		perItem[i] = ks
		allKeys = append(allKeys, ks...)
	}
	priors, err := r.store.LookupPriors(ctx, allKeys)
	if err != nil {
		r.metrics.Observation("lookup_error", "error")
		return nil
	}

	avg := r.cachedGlobalAverage(ctx)
	out := make([]scoredItem, len(items))
	for i, item := range items {
		seederWeight := 1.0
		s := item.Torrent.Seeders()
		if s.Valid {
			// log scales the seeder count so a 1000-seeder torrent
			// ranks well above a 10-seeder one without a
			// 1000:1 multiplier swamping every other feature.
			seederWeight = 1.0 + math.Log1p(float64(s.Uint))
		}
		out[i] = scoredItem{
			origIdx: i,
			score:   r.featureScore(perItem[i], priors, avg) * seederWeight,
		}
	}
	return out
}

// featureScore returns the geometric mean of the per-feature posterior
// expected success values. Geometric (rather than arithmetic) mean
// punishes a single very-low-prior feature more than a clean average
// would: one infamously-bad release group can outweigh several
// average-feature contributions.
//
// Features below the MinObservations floor fall back to the global
// average — without the floor, a single bad observation against a
// fresh release group would mark every torrent from that group
// unfairly.
func (r *Ranker) featureScore(keys []FeatureKey, priors map[FeatureKey]Prior, globalAvg float64) float64 {
	if len(keys) == 0 {
		return globalAvg
	}
	var logSum float64
	var count int
	for _, k := range keys {
		var v float64
		if p, ok := priors[k]; ok && p.Observations() >= int64(r.cfg.MinObservations) {
			v = p.Mean()
		} else {
			v = globalAvg
		}
		// Clamp away from 0 to keep the geometric mean numerically
		// well-defined and to leave room for new features that
		// observe their first failure to keep ranking above zero.
		if v < 0.01 {
			v = 0.01
		}
		logSum += math.Log(v)
		count++
	}
	if count == 0 {
		return globalAvg
	}
	return math.Exp(logSum / float64(count))
}

func (r *Ranker) cachedGlobalAverage(ctx context.Context) float64 {
	now := r.now()

	// Fast path: read-lock and check the cached value's age. Concurrent
	// Rerank callers can all share this read.
	r.avgMu.RLock()
	cached, fetched := r.avgCache, r.avgFetched
	r.avgMu.RUnlock()
	if !fetched.IsZero() && now.Sub(fetched) < r.cacheTTL {
		return cached
	}

	// Slow path: TTL expired (or first call). Take the write lock,
	// double-check that nobody else just refreshed, and call the
	// store at most once per Ranker per TTL window.
	r.avgMu.Lock()
	defer r.avgMu.Unlock()
	if !r.avgFetched.IsZero() && now.Sub(r.avgFetched) < r.cacheTTL {
		return r.avgCache
	}
	avg, err := r.store.GlobalAverage(ctx)
	if err != nil {
		// Keep the previous cache value (defaults to 0.5 on first
		// call) — the global average is nice-to-have, not critical.
		return r.avgCache
	}
	r.avgCache = avg
	r.avgFetched = now
	return avg
}
