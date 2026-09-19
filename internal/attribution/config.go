package attribution

import "strings"

// Config is the operator-tunable surface. Registered as the
// "attribution" configfx section; env prefix ATTRIBUTION_*.
//
// The attribution recon is gated on Enabled — false means the CLI
// command refuses to run with a "feature disabled" error rather
// than producing empty output. Operator opts in deliberately.
type Config struct {
	// Enabled — flip to true to allow the recon CLI to execute.
	// Default false; the CLI refuses to fetch from any *arr until
	// the operator opts in.
	Enabled bool `yaml:"enabled"`

	// Prowlarr — required. Without Prowlarr we have no grab events
	// to start the chain.
	ProwlarrBaseURL string `yaml:"prowlarr_base_url"`
	ProwlarrAPIKey  string `yaml:"prowlarr_api_key"`

	// qBittorrent — optional. Without qB the report skips the
	// download-state column but the rest works.
	QBTBaseURL  string `yaml:"qbt_base_url"`
	QBTUsername string `yaml:"qbt_username"`
	QBTPassword string `yaml:"qbt_password"`

	// *arr endpoints — optional but highly desirable. Without them
	// the report skips the import-outcome column. The recon CLI
	// REUSES wantbridge's *arr URL+key when these aren't set, so
	// the operator doesn't have to double-configure.
	SonarrBaseURL string `yaml:"sonarr_base_url"`
	SonarrAPIKey  string `yaml:"sonarr_api_key"`
	RadarrBaseURL string `yaml:"radarr_base_url"`
	RadarrAPIKey  string `yaml:"radarr_api_key"`
	LidarrBaseURL string `yaml:"lidarr_base_url"`
	LidarrAPIKey  string `yaml:"lidarr_api_key"`

	// HTTPTimeout — per-call timeout against any of the upstream
	// APIs. Default 30s (one Sonarr history fetch can be slow on
	// large libraries).
	HTTPTimeout string `yaml:"http_timeout"`
}

// NewDefaultConfig is safe to ship — recon disabled until operator
// opts in AND configures Prowlarr.
func NewDefaultConfig() Config {
	return Config{
		Enabled:     false,
		HTTPTimeout: "30s",
	}
}

// HasProwlarr reports whether Prowlarr's URL+key are both set. The
// CLI refuses to run without it because there's nothing to anchor
// the chain to.
func (c Config) HasProwlarr() bool {
	return strings.TrimSpace(c.ProwlarrBaseURL) != "" && strings.TrimSpace(c.ProwlarrAPIKey) != ""
}

// HasQB reports whether qB credentials are set.
func (c Config) HasQB() bool {
	return strings.TrimSpace(c.QBTBaseURL) != "" && strings.TrimSpace(c.QBTUsername) != ""
}

// ArrEndpoint returns the (baseURL, apiKey) pair for one *arr
// source, falling back to env-only operators who set the wantbridge
// vars. The CLI factory is responsible for merging from
// wantbridge.Config when these are blank.
func (c Config) ArrEndpoint(s Source) (baseURL, apiKey string) {
	switch s {
	case SourceSonarr:
		return c.SonarrBaseURL, c.SonarrAPIKey
	case SourceRadarr:
		return c.RadarrBaseURL, c.RadarrAPIKey
	case SourceLidarr:
		return c.LidarrBaseURL, c.LidarrAPIKey
	}
	return "", ""
}
