package llmmatch

import (
	"math"
	"testing"
)

func TestConfigRejectsUnsafeConfidenceThreshold(t *testing.T) {
	for _, value := range []float64{
		math.NaN(),
		math.Inf(1),
		-0.1,
		0,
		1.1,
	} {
		cfg := NewDefaultConfig()
		cfg.Enabled = true
		cfg.MinConfidence = value
		if err := cfg.Validate(); err == nil {
			t.Errorf("MinConfidence=%v: expected validation error", value)
		}
	}

	cfg := NewDefaultConfig()
	cfg.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default enabled config must validate: %v", err)
	}
}

func TestConfigRejectsUnboundedMatcherDispatch(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.DailyCallLimit = -1 },
		func(c *Config) { c.MonthlyCallLimit = -1 },
		func(c *Config) { c.MaxRequestBytes = 0 },
		func(c *Config) { c.MaxOutputTokens = 0 },
		func(c *Config) { c.MaxConcurrentCalls = 0 },
		func(c *Config) { c.Timeout = 0 },
	} {
		cfg := NewDefaultConfig()
		cfg.Enabled = true
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Fatal("unsafe dispatch config accepted")
		}
	}
}
