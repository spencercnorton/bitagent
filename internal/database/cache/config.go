package cache

import "time"

// defaultMaxKeys is the MaxKeys value NewDefaultConfig ships, named so
// NewInMemoryCacher can fall back to it when handed a zero.
const defaultMaxKeys = 1000

type Config struct {
	CacheEnabled bool
	EaserEnabled bool
	TTL          time.Duration
	MaxKeys      uint
}

func NewDefaultConfig() Config {
	return Config{
		CacheEnabled: true,
		// The easer has been disabled as it seems to cause a bug whereby zero results are
		// sometimes incorrectly returned; if I can get time to understand the problem better
		// I may open an issue in https://github.com/go-gorm/caches, though they don't seem very
		// responsive to issues, hence why we use a forked version of this library...
		EaserEnabled: false,
		TTL:          time.Minute * 10,
		MaxKeys:      defaultMaxKeys,
	}
}
