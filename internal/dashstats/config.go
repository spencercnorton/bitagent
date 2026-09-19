package dashstats

import "time"

// Config controls the dashboard-stats collector. Registered as "dashstats" on
// the configfx module; env prefix DASHSTATS_*.
type Config struct {
	// Enabled turns the collector on. Defaults TRUE — these are cheap,
	// read-only gauges the dashboard needs out of the box.
	Enabled bool `yaml:"enabled"`

	// Interval between recomputes. The two queries are cheap; 5m keeps the
	// gauges fresh without churn.
	Interval time.Duration `yaml:"interval"`
}

func NewDefaultConfig() Config {
	return Config{
		Enabled:  true,
		Interval: 5 * time.Minute,
	}
}
