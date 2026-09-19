package animedb

import (
	"context"
	"strings"
	"sync"
	"time"
)

// loadRetryInterval throttles snapshot reloads after a failed lazy load, so a
// persistently unavailable DB during heavy classification can't be hammered.
const loadRetryInterval = 30 * time.Second

// Resolver serves deterministic alias -> TMDB lookups from memory. It always
// contains the baked seed set; when constructed with a Store it additionally
// loads the persisted anime_titles snapshot on first use and whenever the
// refresh worker swaps a fresh build in. Seed entries win over data-built rows
// on key collision, so the curated overrides can never be regressed by a data
// refresh.
//
// The zero value is not usable — construct via NewResolver or NewSeedResolver.
type Resolver struct {
	mu             sync.RWMutex
	exact          map[string]Alias
	seedContains   []seedContains
	store          *Store
	metrics        *Metrics
	loaded         bool
	lastLoadFailed time.Time
}

// NewResolver builds a resolver backed by the persisted table. The snapshot is
// loaded lazily on the first Lookup (so construction doesn't depend on the DB
// being migrated yet) and can be refreshed via Swap. metrics may be nil.
func NewResolver(store *Store, metrics *Metrics) *Resolver {
	return &Resolver{
		exact:        seedExactMap(),
		seedContains: seedContainsList(),
		store:        store,
		metrics:      metrics,
	}
}

// NewSeedResolver returns a resolver serving only the baked seed set (no DB).
// Used by the classifier as a guaranteed fallback and by tests.
func NewSeedResolver() *Resolver {
	return &Resolver{
		exact:        seedExactMap(),
		seedContains: seedContainsList(),
		loaded:       true,
	}
}

// Swap installs a freshly built alias set, overlaying the seed entries on top
// (seeds win). Called by the refresh worker/CLI after a successful build so a
// running process picks up new coverage without a restart.
func (r *Resolver) Swap(aliases []Alias) {
	m := make(map[string]Alias, len(aliases)+len(seedEntries)*2)
	for _, a := range aliases {
		if len(a.Normalized) < minNormalizedLen {
			continue
		}
		m[a.Normalized] = a
	}
	for k, v := range seedExactMap() {
		m[k] = v // seeds override data rows
	}
	r.mu.Lock()
	r.exact = m
	r.loaded = true
	r.mu.Unlock()
}

// ensureLoaded lazily loads the DB snapshot once (with a retry throttle on
// failure). A load failure leaves the seed-only map in place.
func (r *Resolver) ensureLoaded() {
	r.mu.RLock()
	done := r.loaded || r.store == nil
	retryAt := r.lastLoadFailed.Add(loadRetryInterval)
	r.mu.RUnlock()
	if done || time.Now().Before(retryAt) {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded || time.Now().Before(r.lastLoadFailed.Add(loadRetryInterval)) {
		return
	}
	rows, err := r.store.LoadAll(context.Background())
	if err != nil {
		r.lastLoadFailed = time.Now()
		return
	}
	m := make(map[string]Alias, len(rows)+len(seedEntries)*2)
	for _, a := range rows {
		if len(a.Normalized) < minNormalizedLen {
			continue
		}
		m[a.Normalized] = a
	}
	for k, v := range seedExactMap() {
		m[k] = v
	}
	r.exact = m
	r.loaded = true
}

// Lookup resolves an anime alias to its canonical TMDB entry. It tries, in
// order: an exact normalized match on the deterministic parsed title, then on
// the LLM-extracted title, then a seed-only substring scan over the whole
// release name (the abbreviation safety net). Any argument may be empty. The
// bool is false when nothing matched.
func (r *Resolver) Lookup(parsedTitle, extractTitle, rawName string) (Alias, bool) {
	r.ensureLoaded()

	r.mu.RLock()
	exact := r.exact
	seeds := r.seedContains
	r.mu.RUnlock()

	for _, key := range []string{parsedTitle, extractTitle} {
		n := Normalize(key)
		if len(n) < minNormalizedLen {
			continue
		}
		if a, ok := exact[n]; ok {
			r.record("exact")
			return a, true
		}
	}

	if rawName != "" && len(seeds) > 0 {
		rn := Normalize(rawName)
		for _, sc := range seeds {
			for _, sub := range sc.subs {
				if strings.Contains(rn, sub) {
					r.record("seed_contains")
					return sc.alias, true
				}
			}
		}
	}

	r.record("miss")
	return Alias{}, false
}

// Size reports how many exact-lookup keys the resolver currently holds
// (seeds + loaded snapshot). Loads the snapshot if not yet loaded.
func (r *Resolver) Size() int {
	r.ensureLoaded()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.exact)
}

func (r *Resolver) record(result string) {
	if r.metrics != nil {
		r.metrics.resolveTotal.WithLabelValues(result).Inc()
	}
}
