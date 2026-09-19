package wantbridge

import (
	"context"
	"time"
)

// Tier classifies a DHT discovery according to upstream demand.
// The dhtcrawler consults Match() on every newly-discovered infohash
// and uses the returned Tier to decide whether to enqueue, prioritise,
// or skip the BEP-9 fetch.
type Tier int

const (
	// Tier0 — high-confidence wantlist match. The BEP-9 fetcher
	// should jump the queue and fetch immediately. Once the metadata
	// arrives, the result is pushed to the matching *arr (see D5
	// push pipeline). Sub-second median latency from discovery →
	// metadata → *arr release add is the target.
	Tier0 Tier = iota

	// Tier1 — no wantlist match, but the hash is otherwise
	// unremarkable. Goes into the normal fetcher queue at the back.
	// Most discoveries land here; this is the steady-state cohort.
	Tier1

	// Tier2 — content-filter would drop this pre-fetch. The name
	// alone is enough to predict a drop (Cyrillic / .iso / NSFW
	// keyword), so we don't bother BEP-9-fetching. Each Tier-2
	// classification saves one BEP-9 round trip.
	//
	// Tier-2 fires only when ContentFilter.Enabled() == true. With
	// the filter disabled, every non-Tier-0 discovery is Tier-1.
	Tier2
)

func (t Tier) String() string {
	switch t {
	case Tier0:
		return "tier0"
	case Tier1:
		return "tier1"
	case Tier2:
		return "tier2"
	default:
		return "unknown"
	}
}

// Source identifies which *arr supplied a wantlist entry. Used as
// a metric label on Match() outcomes so the operator can see which
// service is driving the bridge.
type Source string

const (
	SourceSonarr Source = "sonarr"
	SourceRadarr Source = "radarr"
	SourceLidarr Source = "lidarr"
)

// Kind identifies the canonical-content shape we're matching against.
type Kind int

const (
	KindUnknown Kind = iota
	KindMovie
	KindTV
	KindMusic
)

// Canonical is the normalised shape of a wantlist entry or a torrent
// name. Match logic compares Canonical-of-torrent against
// Canonical-of-wantlist-entries.
//
// For movies: Title + Year is enough.
//
// For TV: Title + Season identifies a season pack; episode-set
// granularity is for fine-grained matching when an episode pack
// arrives but only some episodes are wanted.
//
// For music: Title (artist) + AlbumHint when present.
type Canonical struct {
	Kind   Kind
	Title  string // lowercase, normalised
	Year   int    // 0 if unknown / not applicable
	Season int    // -1 if not a season-shaped match (e.g. movie)
	// EpisodeSet is the set of wanted episodes within the matched
	// season. Empty == "any episode" (i.e. a season pack matches).
	EpisodeSet []int
	AlbumHint  string // music only
}

// MatchResult is what Match() returns for a candidate torrent name.
type MatchResult struct {
	Tier   Tier
	Source Source // populated when Tier == Tier0
	// Canonical is the wantlist entry that matched, when Tier ==
	// Tier0. The dhtcrawler can pass this through to the push
	// pipeline (D5) so the *arr release-add knows which series /
	// movie ID to attach to.
	Canonical Canonical
	// SkipReason is populated when Tier == Tier2. Stable strings
	// for metric labels: "non_latin_script" / "blocked_extension" /
	// "nsfw_keyword".
	SkipReason string
}

// Wantbridge is the top-level service. The factory wires it once
// at startup; the dhtcrawler holds a reference and calls Match()
// on the hot path.
type Wantbridge interface {
	// Match returns the tier classification for a freshly-discovered
	// torrent. Hot-path call; must be lock-free or use a fast RWMutex
	// for read access. The DHT crawler calls this for every new
	// infohash, so naive O(N) is unacceptable at high N.
	Match(name string) MatchResult

	// Enabled reports whether the operator has activated wantbridge.
	// When false, Match() always returns Tier1 (the previous
	// behaviour); callers should still gate any wantbridge-specific
	// metrics emission on this.
	Enabled() bool

	// Enforce reports whether the operator has flipped to live mode.
	// When false (shadow), Match() runs and emits metrics but the
	// dhtcrawler should NOT actually re-prioritise the fetcher
	// queue based on the returned Tier — it's a counterfactual.
	Enforce() bool

	// Snapshot returns wantlist-size statistics for the dashboard
	// and diagnostics. Cheap; reads atomic gauges.
	Snapshot() Snapshot

	// Refresh forces an immediate poll of all *arr sources. Useful
	// from a manual debug endpoint or test harness; not on the hot
	// path.
	Refresh(ctx context.Context) error
}

// Snapshot is the diagnostic view of the wantbridge state.
type Snapshot struct {
	Sources       map[Source]SourceSnapshot
	FingerprintN  int           // total entries in the canonical map
	BloomCapacity int           // bloom filter capacity
	BloomFillRate float64       // 0..1
	LastRebuild   time.Time
}

// SourceSnapshot is per-*arr state.
type SourceSnapshot struct {
	WantlistN      int
	LastPollOK     time.Time
	LastPollErr    time.Time
	ConsecutiveErr int
}

// PushTarget is the data the *arr push pipeline (D5) needs from
// wantbridge to issue a release-add. Returned alongside MatchResult
// for Tier-0 discoveries; the dhtcrawler hands it to the push
// component after BEP-9 + classify completes.
//
// Defined here (rather than in pkg push) to avoid an import cycle:
// wantbridge declares the contract; push consumes it.
type PushTarget struct {
	Source Source

	// Per-source upstream IDs. The push pipeline uses these to
	// route the release-add to the right entity.
	SonarrSeriesID int    // SourceSonarr only
	RadarrMovieID  int    // SourceRadarr only
	LidarrArtistID int    // SourceLidarr only

	Title  string
	Year   int
	Season int  // SourceSonarr only
}
