package dhtcrawler

import "testing"

// TestMetainfoWorkers_LegacyFormulaWhenZero pins the no-op contract:
// when an operator hasn't set Config.MetainfoConcurrency, the in-flight
// cap must match upstream's original 40 × ScalingFactor formula
// exactly. A regression here would silently change concurrency on every
// running deploy that hasn't opted in.
func TestMetainfoWorkers_LegacyFormulaWhenZero(t *testing.T) {
	cases := []struct {
		scalingFactor int
		want          int
	}{
		{1, 40},
		{5, 200},
		{10, 400}, // upstream's default
		{20, 800},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			cfg := Config{MetainfoConcurrency: 0}
			got := metainfoWorkers(cfg, tc.scalingFactor)
			if got != tc.want {
				t.Errorf("metainfoWorkers(0, %d) = %d; want %d (= 40 × ScalingFactor)",
					tc.scalingFactor, got, tc.want)
			}
		})
	}
}

// TestMetainfoWorkers_OverrideWins asserts that any non-zero
// MetainfoConcurrency value short-circuits the legacy formula —
// regardless of ScalingFactor. The operator A/B-tests by env var, so
// the override must dominate.
func TestMetainfoWorkers_OverrideWins(t *testing.T) {
	cases := []struct {
		concurrency   uint
		scalingFactor int
	}{
		// Aggressive throttle (would suggest ~98% wasted handshakes
		// from the 2026-04-24 audit are a per-IP issue worth letting
		// the per-IP rate-limit absorb).
		{50, 10},
		{100, 10},
		{200, 10},
		// Same effective workers as legacy default (10 × 40 = 400) —
		// the override must still take effect even when the value
		// matches what the legacy formula would have returned. Tests
		// the precedence rule independent of value coincidence.
		{400, 10},
		// High value: operator wants headroom regardless of
		// ScalingFactor. Validates we don't silently clamp.
		{1000, 5},
		// Operator turns SF down to 1 but wants 200 metadata workers
		// (uncoupling the two is the whole point of this MR).
		{200, 1},
	}
	for _, tc := range cases {
		t.Run("", func(t *testing.T) {
			cfg := Config{MetainfoConcurrency: tc.concurrency}
			got := metainfoWorkers(cfg, tc.scalingFactor)
			if got != int(tc.concurrency) {
				t.Errorf("metainfoWorkers(%d, %d) = %d; want %d (override must dominate)",
					tc.concurrency, tc.scalingFactor, got, tc.concurrency)
			}
		})
	}
}

// TestMetainfoWorkers_DefaultConfigShape pins the public default. A
// future change to NewDefaultConfig must be deliberate; if someone
// sets MetainfoConcurrency in the default it'd silently change the
// upgrade path for every existing deploy.
func TestMetainfoWorkers_DefaultConfigShape(t *testing.T) {
	cfg := NewDefaultConfig()
	if cfg.MetainfoConcurrency != 0 {
		t.Errorf("NewDefaultConfig must leave MetainfoConcurrency=0 (legacy formula); got %d",
			cfg.MetainfoConcurrency)
	}
}
