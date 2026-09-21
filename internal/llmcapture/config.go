package llmcapture

import "time"

const (
	maxAllowedRows       = 1_000_000
	maxAllowedInputBytes = 1 << 20
)

// Config controls the prospective production-evaluation capture. The feature
// is deliberately disabled by default. When disabled, Recorder returns before
// it initializes the database or performs a privacy lookup. The independent
// expiry janitor (worker key llm_evaluation_capture_janitor, started by
// `worker run --all`) ignores Enabled so disabling collection cannot disable
// the retention guarantee.
type Config struct {
	Enabled         bool          `yaml:"enabled"`
	Retention       time.Duration `yaml:"retention"`
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
	MaxRows         int           `yaml:"max_rows"`
	MaxInputBytes   int           `yaml:"max_input_bytes"`
}

func NewDefaultConfig() Config {
	return Config{
		Enabled:         false,
		Retention:       30 * 24 * time.Hour,
		CleanupInterval: time.Hour,
		MaxRows:         100_000,
		MaxInputBytes:   256 << 10,
	}
}

func (c Config) validate() error {
	if c.Retention <= 0 {
		return ErrInvalidConfig("retention must be positive")
	}
	if c.CleanupInterval <= 0 {
		return ErrInvalidConfig("cleanup_interval must be positive")
	}
	if c.MaxRows <= 0 || c.MaxRows > maxAllowedRows {
		return ErrInvalidConfig("max_rows must be in 1..1000000")
	}
	if c.MaxInputBytes <= 0 || c.MaxInputBytes > maxAllowedInputBytes {
		return ErrInvalidConfig("max_input_bytes must be in 1..1048576")
	}
	return nil
}
