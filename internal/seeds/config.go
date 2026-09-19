// Package seeds runs a scheduled worker that refreshes torrent seeder/leecher
// counts from authoritative public trackers via BEP-15 UDP scrape.
//
// The seeders/leechers that the DHT crawler records
// (torrents_torrent_sources source='dht') are a single-node BEP-33 bloom
// approximation captured at crawl time and never refreshed unless the crawler
// organically re-encounters the hash. They are biased low, quantized, and
// unboundedly stale. A tracker scrape returns the authoritative
// complete/incomplete counts for the whole swarm at trivial bandwidth
// (~70 infohashes per UDP datagram), so this worker is the accuracy fix.
//
// Everything is opt-in and dry-run by default (Enabled=false, EnableWrite=false)
// so the first operational window produces coverage counts — how many of our
// torrents public trackers actually know about — before any row is written.
package seeds

import "time"

// Config controls the seeds refresh worker. Registered as "seeds" on the
// configfx module; env prefix SEEDS_*.
//
// Field-name footgun: the env resolver derives keys via
// strcase.ToSnake(field.Name) and ignores yaml tags, so stacked acronyms and
// digits mis-tokenize (see internal/config/envkey_binding_test.go). Every
// field here is deliberately named so ToSnake yields the intended key —
// notably TrackerUrls (not TrackerURLs, which becomes tracker_ur_ls).
type Config struct {
	// Enabled turns the worker on. When false, no scheduled cycles run.
	Enabled bool `yaml:"enabled"`

	// EnableWrite gates persistence. When false the worker scrapes trackers
	// and emits metrics but writes nothing (dry-run) — flip only after the
	// coverage counts have been reviewed. Env: SEEDS_ENABLE_WRITE.
	EnableWrite bool `yaml:"enable_write"`

	// Interval between refresh cycles. Env: SEEDS_INTERVAL.
	Interval time.Duration `yaml:"interval"`

	// MinRescrapeAge — a hash whose ledger checked_at is younger than this is
	// skipped. Trackers only update swarm counts periodically, so re-scraping
	// more often than this wastes packets. Env: SEEDS_MIN_RESCRAPE_AGE.
	MinRescrapeAge time.Duration `yaml:"min_rescrape_age"`

	// BatchSize caps how many info-hashes one cycle scrapes. Env: SEEDS_BATCH_SIZE.
	BatchSize int `yaml:"batch_size"`

	// Concurrency caps how many trackers are scraped in parallel per batch.
	// Env: SEEDS_CONCURRENCY.
	Concurrency int `yaml:"concurrency"`

	// MaxHashesPerPacket caps info-hashes per BEP-15 scrape datagram. The wire
	// limit is ~74 (typical MTU / 20 bytes per hash); 70 leaves headroom.
	// Env: SEEDS_MAX_HASHES_PER_PACKET.
	MaxHashesPerPacket int `yaml:"max_hashes_per_packet"`

	// ScrapeTimeout bounds one connect+scrape round trip to a single tracker.
	// Env: SEEDS_SCRAPE_TIMEOUT.
	ScrapeTimeout time.Duration `yaml:"scrape_timeout"`

	// PerTrackerInterval is the minimum spacing between successive scrape
	// packets to the same tracker (politeness / anti-ban). Env:
	// SEEDS_PER_TRACKER_INTERVAL.
	PerTrackerInterval time.Duration `yaml:"per_tracker_interval"`

	// TrackerUrls is the pool of public UDP tracker announce URLs to scrape.
	// A hash's result is the max across the whole pool, so a hash present on
	// any one tracker is found. Comma-separated in env: SEEDS_TRACKER_URLS.
	TrackerUrls []string `yaml:"tracker_urls"`
}

// NewDefaultConfig returns conservative, opt-in defaults. The worker is off and
// in dry-run until an operator flips Enabled + EnableWrite after reviewing
// coverage counts.
func NewDefaultConfig() Config {
	return Config{
		Enabled:            false,
		EnableWrite:        false,
		Interval:           1 * time.Hour,
		MinRescrapeAge:     12 * time.Hour,
		BatchSize:          2000,
		Concurrency:        4,
		MaxHashesPerPacket: 70,
		ScrapeTimeout:      15 * time.Second,
		PerTrackerInterval: 1 * time.Second,
		TrackerUrls:        DefaultTrackerUrls(),
	}
}

// DefaultTrackerUrls is the built-in pool of high-uptime public UDP trackers
// that answer scrape requests. Trackers that do not support scrape, or that are
// down, are handled gracefully per-cycle (their error is counted and skipped),
// so a slightly generous list is fine. Override via SEEDS_TRACKER_URLS.
func DefaultTrackerUrls() []string {
	return []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://open.tracker.cl:1337/announce",
		"udp://open.demonii.com:1337/announce",
		"udp://tracker.openbittorrent.com:6969/announce",
		"udp://open.stealth.si:80/announce",
		"udp://exodus.desync.com:6969/announce",
		"udp://tracker.torrent.eu.org:451/announce",
		"udp://explodie.org:6969/announce",
		"udp://opentracker.io:6969/announce",
		"udp://tracker.dler.org:6969/announce",
	}
}
