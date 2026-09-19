package retention

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestDefaultConfigSafe verifies the defaults are "do nothing". A
// new deploy must not immediately start deleting torrents; both
// Enabled and EnablePurge default off.
func TestDefaultConfigSafe(t *testing.T) {
	c := NewDefaultConfig()
	if c.Enabled {
		t.Error("NewDefaultConfig must not enable retention by default")
	}
	if c.EnablePurge {
		t.Error("NewDefaultConfig must not enable real purge by default; dry-run only")
	}
	if c.MaxLastSeen < 90*24*time.Hour {
		t.Errorf("MaxLastSeen default is too aggressive: %v; should be >= 90d to avoid false deletion", c.MaxLastSeen)
	}
	// Per the 2026-04-24 GPT-5.5-pro review: source freshness is not
	// optional. NewDefaultConfig must populate it or the (now hardened)
	// SQL predicate would degenerate when SourceFreshnessMaxAge=0.
	if c.SourceFreshnessMaxAge <= 0 {
		t.Errorf("SourceFreshnessMaxAge must be set to a positive duration; got %v", c.SourceFreshnessMaxAge)
	}
	if c.SourceFreshnessMaxAge > c.MaxLastSeen {
		// If freshness is more lax than the last-seen window, the
		// "fresh confirmed zero" requirement is meaningless — every
		// candidate already passes the looser MaxLastSeen check.
		t.Errorf("SourceFreshnessMaxAge (%v) should be tighter than MaxLastSeen (%v)",
			c.SourceFreshnessMaxAge, c.MaxLastSeen)
	}
}

// TestStartRequiresPositiveFields verifies the worker refuses to
// start with zero thresholds. A bad config should surface as an
// error rather than silently purge everything.
func TestStartRequiresPositiveFields(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"zero interval", func(c *Config) { c.Interval = 0 }},
		{"zero min_age", func(c *Config) { c.MinAge = 0 }},
		{"zero max_last_seen", func(c *Config) { c.MaxLastSeen = 0 }},
		{"zero batch_size", func(c *Config) { c.BatchSize = 0 }},
		{"zero source_freshness_max_age", func(c *Config) { c.SourceFreshnessMaxAge = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewDefaultConfig()
			c.Enabled = true
			c.Interval = time.Hour
			c.MinAge = 24 * time.Hour
			c.MaxLastSeen = 48 * time.Hour
			c.BatchSize = 100
			tc.mut(&c)
			w := &retentionWorker{cfg: c, logger: zap.NewNop().Sugar()}
			err := w.start(nil)
			if err == nil {
				t.Fatal("expected error on invalid config")
			}
		})
	}
}

// TestStartDisabledIsNoop verifies Enabled=false short-circuits
// start without error and without starting a goroutine.
func TestStartDisabledIsNoop(t *testing.T) {
	w := &retentionWorker{cfg: NewDefaultConfig(), logger: zap.NewNop().Sugar()}
	if err := w.start(nil); err != nil {
		t.Fatalf("disabled retention should not error on start: %v", err)
	}
	if w.cancel != nil {
		t.Fatal("disabled retention should not have installed a cancel func")
	}
}

// TestMetricsRegister verifies all collectors are non-nil and stable.
// Catches drift where a new metric is added to the struct but not
// exported via Collectors().
func TestMetricsRegister(t *testing.T) {
	m := NewMetrics()
	cs := m.Collectors()
	const expected = 7
	if len(cs) != expected {
		t.Fatalf("expected %d collectors, got %d", expected, len(cs))
	}
	for i, c := range cs {
		if c == nil {
			t.Errorf("collector %d is nil", i)
		}
	}
}
