package csamblocklist

import (
	"context"
	"time"

	"github.com/spencercnorton/bitagent/internal/protocol"
)

// Manager is the pre-fetch CSAM blocklist. The DHT crawler holds a
// reference and consults it before any BEP-9 fetch.
//
// All methods are safe for concurrent use. Lookup must be O(1) on the
// hot path — implementations use an in-memory bloom filter keyed by
// the SHA-256 double-hash of the SHA-1 infohash.
type Manager interface {
	// IsBlocked returns true iff the infohash's double-hash is in the
	// loaded blocklist. Hot-path call; must not allocate or block.
	IsBlocked(infoHash protocol.ID) bool

	// Filter returns the subset of infoHashes whose double-hash is
	// NOT in the blocklist. Used by infohash_triage as a batch shim
	// parallel to BlockingManager.Filter — same signature shape so
	// the call sites read consistently.
	Filter(infoHashes []protocol.ID) []protocol.ID

	// Refresh forces an immediate re-fetch of all configured feeds.
	// Called periodically by a worker; can also be invoked from a
	// debug endpoint for operator forcing. Returns the first feed
	// error encountered, or nil. Partial success is normal — feeds
	// that fail leave the previous loaded state intact for that feed.
	Refresh(ctx context.Context) error

	// Snapshot returns operator-visible diagnostics. Cheap; reads
	// atomic gauges + a copy of the per-feed status map.
	Snapshot() Snapshot

	// Enabled reports whether the manager is doing real work. Returns
	// false for NoOp; true for the live Service even before the first
	// successful refresh.
	Enabled() bool
}

// Snapshot is the diagnostic view of the blocklist state.
type Snapshot struct {
	// EntryCount is the total number of double-hashes currently
	// loaded across all feeds. Bloom filter capacity sets a soft
	// upper bound; this value is the actual fill.
	EntryCount int

	// BloomCapacity is the configured bloom filter capacity.
	BloomCapacity int

	// BloomFalsePositiveRate is the configured target FPR.
	BloomFalsePositiveRate float64

	// LastRefreshAt is when Refresh() last completed, regardless of
	// per-feed outcomes. Zero before the first refresh.
	LastRefreshAt time.Time

	// PerFeed is keyed by feed URL.
	PerFeed map[string]FeedStatus
}

// FeedStatus is the per-feed diagnostic view.
type FeedStatus struct {
	URL                string
	LastSuccessAt      time.Time
	LastError          string
	LastErrorAt        time.Time
	ConsecutiveErrs    int
	EntriesContributed int
}
