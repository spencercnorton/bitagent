package csamblocklist

import (
	"os"
	"path/filepath"
	"time"
)

// Config is the operator-tunable surface for the CSAM blocklist.
// Registered as the "csam_blocklist" configfx section; env prefix
// CSAM_BLOCKLIST_*.
//
// Defaults: ENABLED with no feeds configured = NoOp until operator
// opts in. Self-export ENABLED to a local JSONL file; outbound POST
// is OFF by default and requires an explicit upstream URL.
//
// The split (Enabled-but-empty-feeds vs disabled) exists so the
// pre-fetch infrastructure is permanently wired in the dhtcrawler.
// Operators don't need a code change to opt into community feeds —
// just env vars.
type Config struct {
	// Enabled — turn the blocklist on. When false, the package is a
	// pure no-op: no feeds polled, no metrics emitted, IsBlocked
	// always returns false. Default true; the safety case is strong
	// enough that the runtime cost (one bloom filter test per
	// discovery, no allocations) is justified.
	Enabled bool `yaml:"enabled"`

	// FeedUrls — space-separated list of community-feed URLs. Each
	// feed is fetched on startup and re-fetched at FeedRefreshInterval.
	// Format: newline-delimited 64-char lowercase hex (one
	// double-hash per line); leading/trailing whitespace ignored;
	// '#' lines are comments. The wire format is documented in
	// docs/csam-defense.md and examples/csam-feed-format.md.
	//
	// Default empty — operator opts in. With no feeds configured the
	// service runs in degenerate "always allow" mode and is
	// effectively a NoOp (one extra bloom-filter test on the hot
	// path checking an empty filter).
	//
	// NOTE on the field-name spelling (FeedUrls, not FeedURLs): the
	// config env resolver derives the env key from the Go FIELD NAME
	// via strcase.ToSnake (see internal/config/config.go), NOT from
	// the yaml tag. "FeedURLs" tokenizes as Feed+UR+Ls → "feed_ur_ls"
	// → CSAM_BLOCKLIST_FEED_UR_LS, which does NOT match the documented
	// CSAM_BLOCKLIST_FEED_URLS (docs/csam-defense.md,
	// docs/configuration.md, examples/docker-compose.public.yml).
	// "FeedUrls" tokenizes as Feed+Urls → "feed_urls" →
	// CSAM_BLOCKLIST_FEED_URLS (the documented name). Do not "correct"
	// the casing back to FeedURLs — it silently breaks the env binding,
	// so an operator opting into a CSAM feed via env would be ignored.
	FeedUrls []string `yaml:"feed_urls"`

	// FeedRefreshInterval — how often to re-poll all feeds. Default
	// 6h. Lower values pick up community updates faster at the cost
	// of feed-server load; higher values risk staleness. The
	// community-feed cycle is expected to be in days, not minutes,
	// so 6h is well-matched.
	FeedRefreshInterval time.Duration `yaml:"feed_refresh_interval"`

	// FeedFetchTimeout — per-feed HTTP timeout. Default 30s. Slow
	// feeds time out and the previous loaded state for that feed is
	// retained.
	FeedFetchTimeout time.Duration `yaml:"feed_fetch_timeout"`

	// FeedMaxBytes — per-feed response-size cap. Default 64 MiB,
	// which holds ~1M double-hash entries (64 chars + newline = 65
	// bytes/entry). Defends against runaway feeds.
	FeedMaxBytes int64 `yaml:"feed_max_bytes"`

	// BloomCapacity — sized for the union of all feeds. Default 1M.
	// At 0.001 FPR this is ~14 MiB of RAM; on a typical deployment
	// (1-10k entries) the actual footprint is dominated by the
	// configured-capacity allocation, not the data.
	BloomCapacity uint `yaml:"bloom_capacity"`

	// BloomFalsePositiveRate — target FPR. Default 0.001 (0.1%). A
	// false positive in this filter rejects a non-CSAM infohash that
	// happens to collide; given the safety stakes this is the
	// preferred error direction. The bloom filter is rebuilt on
	// every refresh, so an FP doesn't persist across restarts.
	BloomFalsePositiveRate float64 `yaml:"bloom_false_positive_rate"`

	// ExportEnabled — when true (default), CSAM observations from
	// the post-fetch classifier are double-hashed and appended to
	// the local JSONL log at ExportFilePath. The log is the
	// operator's own record; it never leaves the host unless
	// ExportUpstreamURL is set.
	ExportEnabled bool `yaml:"export_enabled"`

	// ExportFilePath — local JSONL log of double-hashed observations.
	// Each line is one JSON object:
	//   {"ts":"<RFC3339>","double_hash":"<64-hex>","reason":"banned_keyword"}
	// The infohash itself is never written.
	//
	// The path is used VERBATIM by the exporter (export.go appendLocal
	// does os.MkdirAll(filepath.Dir(path)) + open) — nothing resolves
	// it against a data root, so a relative value is interpreted
	// relative to the process CWD. NewDefaultConfig therefore prefers
	// an absolute path under $XDG_CONFIG_HOME when that is set (see
	// NewDefaultConfig); an explicit CSAM_BLOCKLIST_EXPORT_FILE_PATH
	// env override always wins over the computed default.
	ExportFilePath string `yaml:"export_file_path"`

	// ExportUpstreamURL — optional HTTPS endpoint to POST each
	// observation to. Default empty (off). When set, the post body
	// is the same JSON object format as the JSONL line. Operator
	// opts in via env. Failures are logged + counted but do not
	// affect the local export.
	ExportUpstreamURL string `yaml:"export_upstream_url"`

	// ExportUpstreamAuthHeader — optional Authorization header value
	// for the upstream POST. Format: full header value, e.g.
	// "Bearer <token>". Empty disables the header.
	ExportUpstreamAuthHeader string `yaml:"export_upstream_auth_header"`

	// ExportUpstreamTimeout — per-POST HTTP timeout. Default 10s.
	ExportUpstreamTimeout time.Duration `yaml:"export_upstream_timeout"`
}

// NewDefaultConfig returns the safe-defaults config: enabled with no
// feeds (effective no-op until operator opts in), self-export to
// local JSONL on, no outbound POST.
func NewDefaultConfig() Config {
	return Config{
		Enabled:                true,
		FeedUrls:               nil,
		FeedRefreshInterval:    6 * time.Hour,
		FeedFetchTimeout:       30 * time.Second,
		FeedMaxBytes:           64 * 1024 * 1024,
		BloomCapacity:          1_000_000,
		BloomFalsePositiveRate: 0.001,

		ExportEnabled:            true,
		ExportFilePath:           defaultExportFilePath(),
		ExportUpstreamURL:        "",
		ExportUpstreamAuthHeader: "",
		ExportUpstreamTimeout:    10 * time.Second,
	}
}

// defaultExportFilePath picks a write-safe default for the local
// observation log.
//
// The exporter uses ExportFilePath verbatim (no data-root resolution),
// so a relative default like "data/..." is interpreted against the
// process CWD. The production image has no WORKDIR (CWD = "/") and
// runs as a non-root uid, so os.MkdirAll("data", …) fails with
// "permission denied" on every observation. To make the out-of-box
// default actually writable for non-root images, prefer an absolute
// path under $XDG_CONFIG_HOME when that is set — in the production
// container XDG_CONFIG_HOME is "/config", a writable mounted volume.
//
// When XDG_CONFIG_HOME is unset (e.g. a dev `go run` from the repo
// root) we keep the legacy relative "data/..." path, which is writable
// there. An explicit CSAM_BLOCKLIST_EXPORT_FILE_PATH env override is
// applied by configfx after this default and therefore always wins.
func defaultExportFilePath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "csam", "csam-double-hashes.jsonl")
	}
	return "data/csam-double-hashes.jsonl"
}

// HasAnyFeed reports whether at least one feed URL is configured.
// Used by the factory to decide between Service and NoOp.
func (c Config) HasAnyFeed() bool {
	for _, u := range c.FeedUrls {
		if u != "" {
			return true
		}
	}
	return false
}
