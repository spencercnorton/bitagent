package wantbridge

// Config is the operator-tunable surface for the wantbridge service.
// Registered as the "wantbridge" configfx section in a follow-up MR;
// env prefix WANTBRIDGE_*.
//
// Defaults are conservative: feature off entirely. The two-stage
// opt-in (Enabled → Enforce) mirrors contentfilter / peerrep /
// retention.
type Config struct {
	// Enabled — turn the bridge on. When false, the package is a
	// pure no-op: no *arr polling, no fingerprint maintenance,
	// no Match() work, no metrics emitted. Default false so a
	// fresh deploy is observably indistinguishable from
	// pre-wantbridge.
	Enabled bool `yaml:"enabled"`

	// Enforce — flip from shadow to live. With Enforce=false but
	// Enabled=true, Match() runs and metrics populate, but the
	// dhtcrawler does NOT actually re-prioritise the fetcher queue
	// or skip Tier-2 discoveries. Counterfactual measurement only.
	// Default false so the operator validates the match
	// distribution before any behaviour change.
	Enforce bool `yaml:"enforce"`

	// PollInterval — how often to re-fetch each *arr's wantlist.
	// Default 5m. Lower values increase *arr API load; higher
	// values mean a newly-monitored title takes longer to start
	// receiving prioritised fetches. 5m is the sweet spot for the
	// operator's stack.
	PollInterval string `yaml:"poll_interval"`

	// SonarrBaseURL / SonarrAPIKey — Sonarr instance. Empty disables
	// Sonarr polling. Both must be set for Sonarr to be live.
	SonarrBaseURL string `yaml:"sonarr_base_url"`
	SonarrAPIKey  string `yaml:"sonarr_api_key"`

	// Radarr / Lidarr — same shape.
	RadarrBaseURL string `yaml:"radarr_base_url"`
	RadarrAPIKey  string `yaml:"radarr_api_key"`
	LidarrBaseURL string `yaml:"lidarr_base_url"`
	LidarrAPIKey  string `yaml:"lidarr_api_key"`

	// BloomCapacity — sized for the union of all wantlist entries
	// expected. Default 100k. The operator's stack typically tracks
	// 500-2000 series + 100-500 movies + 10-50 artists, so 100k
	// gives 50× headroom for episode-level granularity. Bloom is
	// rebuilt on every successful poll.
	BloomCapacity int `yaml:"bloom_capacity"`

	// BloomFalsePositiveRate — target rate. 0.001 (0.1%) by default.
	// Low FPR means the slow-path exact-match table fires only on
	// real candidates; high FPR means more wasted exact matches.
	// 0.001 with capacity 100k uses ~150KB of RAM — negligible.
	BloomFalsePositiveRate float64 `yaml:"bloom_false_positive_rate"`

	// MinTitleTokens — minimum word count in a torrent's normalised
	// title before we attempt a wantlist match. Default 1 — single-
	// word titles like "Inception", "Avatar", "Up" are valid. Set
	// to 2 if the operator is seeing too many false positives on
	// generic single-token titles.
	MinTitleTokens int `yaml:"min_title_tokens"`
}

// NewDefaultConfig returns a config that's safe to ship enabled in
// shadow mode (every match counts but nothing actually re-prioritises).
// Defaults match the operator's 2026-04-25 ask.
func NewDefaultConfig() Config {
	return Config{
		Enabled: false,
		Enforce: false,

		PollInterval: "5m",

		SonarrBaseURL: "",
		SonarrAPIKey:  "",
		RadarrBaseURL: "",
		RadarrAPIKey:  "",
		LidarrBaseURL: "",
		LidarrAPIKey:  "",

		BloomCapacity:          100_000,
		BloomFalsePositiveRate: 0.001,
		MinTitleTokens:         1,
	}
}

// HasAnySource reports whether at least one *arr is configured. The
// factory uses this to short-circuit construction when no source is
// reachable — wantbridge with zero sources is a deterministic
// no-op (every Match() returns Tier1 because the wantlist is empty).
func (c Config) HasAnySource() bool {
	return (c.SonarrBaseURL != "" && c.SonarrAPIKey != "") ||
		(c.RadarrBaseURL != "" && c.RadarrAPIKey != "") ||
		(c.LidarrBaseURL != "" && c.LidarrAPIKey != "")
}
