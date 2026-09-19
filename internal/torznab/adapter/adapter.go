package adapter

import (
	"context"
	"time"

	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/torznab"
)

// LivenessFilter is the optional dependency the adapter uses to
// drop dead-infohash entries from a search response. Implemented in
// production by *liveness.Store; nil disables the filter (zero
// overhead).
type LivenessFilter interface {
	// DeadSet returns the subset of supplied info_hashes that are
	// currently marked dead. The map is keyed by the hex rendering
	// of the info_hash.
	DeadSet(ctx context.Context, infoHashes [][]byte) (map[string]struct{}, error)
}

// LivenessMetrics is the optional metrics sink used to count items
// dropped by the filter. Implemented in production by
// *liveness.LivenessMetrics; nil suppresses metric increments.
type LivenessMetrics interface {
	TorznabExcluded()
}

// VerdictsFilter is the optional phase-B verdict-ledger reader
// (docs/design/verdict-ledger.md §4): implemented in production by
// *verdicts.Store; nil disables the consult entirely.
type VerdictsFilter interface {
	// BlockedSet returns the subset of supplied info_hashes whose
	// current ledger verdict excludes them from serving
	// (quarantined/blacklisted/tombstoned), keyed by lowercase hex.
	BlockedSet(ctx context.Context, infoHashes [][]byte) (map[string]struct{}, error)
}

// VerdictsMetrics receives reader observations. Implemented in production
// by *verdicts.Metrics; nil suppresses increments.
type VerdictsMetrics interface {
	Reader(reader, outcome string, n int)
}

// Reranker is the optional dependency the adapter uses to reorder
// search results by historical grab-success priors. Implemented in
// production by *priors.Ranker; nil disables ranking entirely (zero
// overhead). The implementation must be safe for concurrent calls
// and must never return an error — ranking is best-effort, and the
// adapter falls back to the original order on any internal failure
// so search availability dominates.
type Reranker interface {
	Rerank(ctx context.Context, items []search.TorrentContentResultItem) []search.TorrentContentResultItem
}

// FreshnessConfig drives the seeder-confidence filter applied after
// liveness. Construct via FreshnessConfigFromTorznab(cfg) or build
// manually for tests.
type FreshnessConfig struct {
	// HideZeroSeeders skips items whose Seeders() is both Valid AND
	// equal to 0 — every scraped tracker source said zero. Without
	// this filter *arrs queue these and the grab sits in metaDL.
	HideZeroSeeders bool

	// HideUnknownSeedersAgeDays skips items where Seeders() is
	// invalid (no source ever returned scraped seed counts) AND the
	// most-recent source row is older than this many days. Zero
	// disables the staleness check entirely; non-stale unknowns are
	// still surfaced because a fresh DHT discovery is signal.
	HideUnknownSeedersAgeDays int

	// AuthoritativeZeroOnly restricts the zero-hide to the 'tracker'
	// source row (mirrored from the torrent_tracker_seeds ledger by
	// internal/seeds). The DHT BEP-33 bloom count is a single-node
	// approximation that reads 0 for many young alive swarms; without
	// this, HideZeroSeeders suppresses freshly-crawled valid content.
	// A bloom-only zero is instead treated as honest-unknown: served
	// while any source row is recent, aged out by the staleness
	// window, and reported with no seeders attr (see search_result.go).
	AuthoritativeZeroOnly bool
}

// IsZero reports whether the config disables both filters. Lets the
// hot-path skip the post-search loop entirely.
func (f FreshnessConfig) IsZero() bool {
	return !f.HideZeroSeeders && f.HideUnknownSeedersAgeDays <= 0
}

// FreshnessConfigFromTorznab translates the operator-facing
// torznab.Config into the adapter-internal struct.
func FreshnessConfigFromTorznab(cfg torznab.Config) FreshnessConfig {
	return FreshnessConfig{
		HideZeroSeeders:           cfg.HideZeroSeeders,
		HideUnknownSeedersAgeDays: cfg.HideUnknownSeedersAgeDays,
		AuthoritativeZeroOnly:     cfg.ZeroSeedersAuthoritativeOnly,
	}
}

func New(search search.Search) Adapter {
	return Adapter{
		search: search,
	}
}

// NewWithLiveness builds an adapter wired to a liveness filter. If
// either filter or enabled is nil/false the adapter behaves
// identically to New() — no extra round trip per request.
func NewWithLiveness(s search.Search, filter LivenessFilter, enabled bool, metrics LivenessMetrics) Adapter {
	a := Adapter{search: s, livenessMetrics: metrics}
	if enabled {
		a.liveness = filter
	}
	return a
}

// NewWithFilters wires both the liveness and freshness filters. The
// freshness filter has no I/O so it's always applied when non-zero.
func NewWithFilters(
	s search.Search,
	filter LivenessFilter,
	livenessEnabled bool,
	livenessMetrics LivenessMetrics,
	freshness FreshnessConfig,
) Adapter {
	a := NewWithLiveness(s, filter, livenessEnabled, livenessMetrics)
	a.freshness = freshness
	return a
}

// NewWithFiltersAndRanker wires the liveness filter, freshness
// filter, AND the priors-based reranker. The reranker runs last —
// after dead and zero-seeder items have been dropped — so it only
// reorders the surviving candidates. Pass a nil ranker to behave
// identically to NewWithFilters.
func NewWithFiltersAndRanker(
	s search.Search,
	filter LivenessFilter,
	livenessEnabled bool,
	livenessMetrics LivenessMetrics,
	freshness FreshnessConfig,
	ranker Reranker,
) Adapter {
	a := NewWithFilters(s, filter, livenessEnabled, livenessMetrics, freshness)
	a.reranker = ranker
	return a
}

// WithVerdicts wires the phase-B verdict-ledger consult. While live is
// false the ledger is SHADOW-metered only: divergences against the
// liveness path are counted (bitagent_verdicts_reader_total) but serving is
// unchanged. live=true additionally excludes ledger-blocked items — a
// strict union with the liveness exclusion, never a replacement, so
// nothing the liveness path would drop is ever resurrected.
func (a Adapter) WithVerdicts(filter VerdictsFilter, live bool, metrics VerdictsMetrics) Adapter {
	a.verdicts = filter
	a.verdictsLive = live
	a.verdictsMetrics = metrics
	return a
}

type Adapter struct {
	search          search.Search
	liveness        LivenessFilter
	livenessMetrics LivenessMetrics
	verdicts        VerdictsFilter
	verdictsLive    bool
	verdictsMetrics VerdictsMetrics
	freshness       FreshnessConfig
	reranker        Reranker
	// now is injected for deterministic staleness tests; production
	// callers use the zero value, which falls back to time.Now.
	now func() time.Time
}

func (a Adapter) Search(ctx context.Context, req torznab.SearchRequest) (torznab.SearchResult, error) {
	options := []query.Option{search.TorrentContentDefaultOption(), query.WithTotalCount(false)}

	reqOptions, reqErr := searchRequestToQueryOptions(req)
	if reqErr != nil {
		return torznab.SearchResult{}, reqErr
	}

	options = append(options, reqOptions...)

	searchResult, searchErr := a.search.TorrentContent(ctx, options...)
	if searchErr != nil {
		return torznab.SearchResult{}, searchErr
	}

	if a.liveness != nil || a.verdicts != nil {
		filtered, ferr := a.applyLivenessFilter(ctx, searchResult)
		if ferr == nil {
			searchResult = filtered
		}
		// On filter error fall through to the unfiltered response.
		// Availability of search dominates; a dead-set lookup miss
		// is not a reason to fail the request.
	}

	if !a.freshness.IsZero() {
		searchResult = a.applyFreshnessFilter(searchResult)
	}

	if a.reranker != nil {
		// Rerank operates in-place on the items slice; we do not
		// catch a panic here because the production implementation
		// (*priors.Ranker) treats every internal failure as a
		// no-op. If a ranker did panic we'd want to find out about
		// it loudly, not swallow it.
		searchResult.Items = a.reranker.Rerank(ctx, searchResult.Items)
	}

	return torrentContentResultToTorznabResult(req, searchResult, a.freshness.AuthoritativeZeroOnly), nil
}

// applyFreshnessFilter drops dead-confidence items: every-source
// zero-seeder rows, and unknown-seeder rows where every source is
// older than the staleness window. Pure CPU — no DB round trip.
func (a Adapter) applyFreshnessFilter(res search.TorrentContentResult) search.TorrentContentResult {
	if len(res.Items) == 0 {
		return res
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	var staleCutoff time.Time
	if a.freshness.HideUnknownSeedersAgeDays > 0 {
		staleCutoff = now.AddDate(0, 0, -a.freshness.HideUnknownSeedersAgeDays)
	}

	kept := make([]search.TorrentContentResultItem, 0, len(res.Items))
	for _, item := range res.Items {
		if a.dropDeadConfidence(item, staleCutoff) {
			if a.livenessMetrics != nil {
				a.livenessMetrics.TorznabExcluded()
			}
			continue
		}
		kept = append(kept, item)
	}
	res.Items = kept
	return res
}

// dropDeadConfidence applies the zero-seeder and stale-unknown rules
// to a single item.
func (a Adapter) dropDeadConfidence(item search.TorrentContentResultItem, staleCutoff time.Time) bool {
	if a.freshness.AuthoritativeZeroOnly {
		// Three-state read (honest-unknown principle): a 0 is real only
		// when the seeds worker wrote it from a tracker scrape. A
		// bloom-only zero is honest-unknown — same treatment as no
		// seeder data at all: served while recent, aged out when stale.
		tracker := trackerSeeders(item.Torrent.Sources)
		if a.freshness.HideZeroSeeders && tracker.Valid && tracker.Uint == 0 {
			return true
		}
		if a.freshness.HideUnknownSeedersAgeDays > 0 && !tracker.Valid {
			all := item.Torrent.Seeders()
			if (!all.Valid || all.Uint == 0) && isAllSourcesStale(item.Torrent.Sources, staleCutoff) {
				return true
			}
		}
		return false
	}
	// Legacy (pre-v0.41): any Valid 0, including the DHT bloom
	// approximation, counts as a real zero.
	seeders := item.Torrent.Seeders()
	if a.freshness.HideZeroSeeders && seeders.Valid && seeders.Uint == 0 {
		return true
	}
	if a.freshness.HideUnknownSeedersAgeDays > 0 && !seeders.Valid {
		return isAllSourcesStale(item.Torrent.Sources, staleCutoff)
	}
	return false
}

// trackerSeeders returns the seed count from the authoritative
// 'tracker' source row, or an invalid NullUint when the hash has no
// tracker verdict.
func trackerSeeders(sources []model.TorrentsTorrentSource) model.NullUint {
	for _, src := range sources {
		if src.Source == model.SourceKeyTracker && src.Seeders.Valid {
			return src.Seeders
		}
	}
	return model.NullUint{}
}

// isAllSourcesStale reports whether every source row predates the
// cutoff. An item with no sources at all is treated as stale; we have
// no signal that anyone has seen it recently.
func isAllSourcesStale(sources []model.TorrentsTorrentSource, cutoff time.Time) bool {
	if len(sources) == 0 {
		return true
	}
	for _, src := range sources {
		latest := src.UpdatedAt
		if src.PublishedAt.Valid && src.PublishedAt.Time.After(latest) {
			latest = src.PublishedAt.Time
		}
		if latest.After(cutoff) {
			return false
		}
	}
	return true
}

// applyLivenessFilter drops items whose info_hash is currently dead, and —
// phase B — consults the verdict ledger on the same hash list (one extra
// indexed round trip). Ledger semantics: shadow-metered while
// VERDICTS_READERS_ENABLED=false; when live, ledger-blocked items are
// excluded IN UNION with the dead set (never instead of it). A ledger
// lookup error is advisory — count it and serve the liveness-only result
// (availability dominates; the ledger is a curator, not a dependency).
func (a Adapter) applyLivenessFilter(ctx context.Context, res search.TorrentContentResult) (search.TorrentContentResult, error) {
	if len(res.Items) == 0 {
		return res, nil
	}
	hashes := make([][]byte, 0, len(res.Items))
	for _, item := range res.Items {
		hashes = append(hashes, item.Torrent.InfoHash.Bytes())
	}
	var dead map[string]struct{}
	if a.liveness != nil {
		var err error
		dead, err = a.liveness.DeadSet(ctx, hashes)
		if err != nil {
			return res, err
		}
	}
	var blocked map[string]struct{}
	if a.verdicts != nil {
		var verr error
		blocked, verr = a.verdicts.BlockedSet(ctx, hashes)
		if verr != nil {
			if a.verdictsMetrics != nil {
				a.verdictsMetrics.Reader("torznab", "error", 1)
			}
			blocked = nil
		}
	}
	if len(dead) == 0 && len(blocked) == 0 {
		return res, nil
	}
	kept := make([]search.TorrentContentResultItem, 0, len(res.Items))
	for _, item := range res.Items {
		key := hashKey(item.Torrent.InfoHash.Bytes())
		_, isDead := dead[key]
		_, isBlocked := blocked[key]
		// Divergence metering is SHADOW-ONLY, mirroring the crawler
		// reader's mutually-exclusive live/shadow outcomes: post-flip the
		// only torznab outcome is 'excluded', so the shadow counters go
		// quiet instead of silently changing meaning.
		if a.verdictsMetrics != nil && !a.verdictsLive {
			switch {
			case isBlocked && isDead:
				a.verdictsMetrics.Reader("torznab", "both", 1)
			case isBlocked:
				// The resurrection cohort: the ledger would exclude
				// this (durable blacklist/quarantine) but liveness has
				// been reset by a MarkAlive — the divergence the
				// shadow-review gate reads before a flag flip.
				a.verdictsMetrics.Reader("torznab", "ledger_only", 1)
			case isDead:
				a.verdictsMetrics.Reader("torznab", "liveness_only", 1)
			}
		}
		if isDead {
			if a.livenessMetrics != nil {
				a.livenessMetrics.TorznabExcluded()
			}
			continue
		}
		if isBlocked && a.verdictsLive {
			if a.verdictsMetrics != nil {
				a.verdictsMetrics.Reader("torznab", "excluded", 1)
			}
			continue
		}
		kept = append(kept, item)
	}
	res.Items = kept
	return res, nil
}

// hashKey mirrors liveness.HashKey without an import dependency on
// that package — the adapter knows nothing about liveness internals
// beyond the interface contract.
func hashKey(infoHash []byte) string {
	const hexAlphabet = "0123456789abcdef"
	out := make([]byte, len(infoHash)*2)
	for i, b := range infoHash {
		out[2*i] = hexAlphabet[b>>4]
		out[2*i+1] = hexAlphabet[b&0x0f]
	}
	return string(out)
}
