// Package retention runs a scheduled worker that purges raw,
// unlabelled infohash rows per the policy in ARCHITECTURE.md §Phase
// 6. Dry-run is the default. Real deletion is opt-in per config flag
// so the first operational window produces counts-only data against
// which the thresholds can be tuned before any row is actually
// removed.
package retention

import "time"

// Config controls retention behaviour. Every threshold is expressed
// at the most conservative usable value; tune downward with real
// observation data.
//
// Registered as "retention" on the configfx module; env prefix
// RETENTION_*.
type Config struct {
	// Enabled turns the worker on. When false, no scheduled cycles
	// run and no metrics are emitted.
	Enabled bool `yaml:"enabled"`

	// EnablePurge gates whether we actually delete. Default false —
	// the worker will emit
	// bitagent_retention_would_purge_total but not touch the DB.
	// Flip this only after dry-run counts have been reviewed.
	EnablePurge bool `yaml:"enable_purge"`

	// Interval between retention cycles.
	Interval time.Duration `yaml:"interval"`

	// MinAge is the minimum time since torrents.created_at for a
	// row to be a purge candidate. Prevents us from removing
	// freshly-discovered torrents before the evidence ingestor
	// could realistically observe them.
	MinAge time.Duration `yaml:"min_age"`

	// MaxLastSeen — a torrent is a candidate only if its
	// torrents.updated_at is older than this many days. This is
	// our proxy for last_seen_at: the crawler bumps updated_at
	// on every re-observation.
	MaxLastSeen time.Duration `yaml:"max_last_seen"`

	// BatchSize caps how many rows one cycle considers and purges.
	// Protects against an unbounded statement that locks the
	// torrents table during a long delete.
	BatchSize int `yaml:"batch_size"`

	// SourceFreshnessMaxAge bounds how recent a
	// torrents_torrent_sources row must be for its seeder reading to
	// count as authoritative evidence of deadness. A source row whose
	// updated_at is older than this is treated as STALE — the torrent
	// is kept regardless of what that row reported.
	//
	// Per the GPT-5.5-pro review of the 2026-04-24 efficiency report:
	// the original predicate (`COALESCE(seeders,0)=0`) treated three
	// distinct states as a single "dead" signal — observed-zero,
	// never-measured, and once-measured-but-now-stale. That misclass
	// would purge torrents whose tracker happened to be offline at
	// scrape time, or whose source had simply not been re-checked.
	//
	// The hardened predicate (worker.go) now requires:
	//   1. at least one source row reporting non-NULL seeders == 0
	//   2. that row's updated_at is within SourceFreshnessMaxAge
	//   3. NO row reports positive seeders, NULL seeders, OR
	//      stale-and-zero — any of those three keeps the torrent.
	SourceFreshnessMaxAge time.Duration `yaml:"source_freshness_max_age"`
}

// NewDefaultConfig returns conservative defaults. These match
// ARCHITECTURE.md §3.4 "start 180d, multiple stale observations".
// Observations will pull these values down.
func NewDefaultConfig() Config {
	return Config{
		Enabled:               false, // explicit opt-in
		EnablePurge:           false, // dry-run until operator flips it
		Interval:              6 * time.Hour,
		MinAge:                60 * 24 * time.Hour,  // 60 days
		MaxLastSeen:           180 * 24 * time.Hour, // 180 days
		BatchSize:             5000,
		SourceFreshnessMaxAge: 30 * 24 * time.Hour, // 30 days
	}
}
