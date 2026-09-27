package tmdb

import "time"

type Config struct {
	Enabled        bool
	BaseURL        string
	APIKey         string
	RateLimit      time.Duration
	RateLimitBurst int
}

func NewDefaultConfig() Config {
	return Config{
		Enabled:        true,
		BaseURL:        "https://api.themoviedb.org/3",
		APIKey:         "",
		RateLimit:      defaultRateLimit,
		RateLimitBurst: defaultRateLimitBurst,
	}
}

const (
	defaultRateLimit      = time.Second / 20
	defaultRateLimitBurst = 5
)
