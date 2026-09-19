package wantbridge

import (
	"context"
	"testing"
)

// Scaffold-level tests. The real wantbridge implementation lands in
// a follow-up MR with a comprehensive test suite (bloom collisions,
// canonical normalisation, *arr poller mock, queue-priority shim).
// For now we just pin the NoOp contract so future implementations
// can't accidentally regress the "wantbridge disabled is a pure
// no-op" promise.

func TestNoOp_MatchReturnsTier1(t *testing.T) {
	// NoOp must always return Tier1 — never Tier0 (would falsely
	// claim a wantlist match) or Tier2 (would falsely claim a
	// pre-fetch skip).
	w := New(NewDefaultConfig())
	for _, name := range []string{
		"",
		"Some Movie 2024",
		"The.Wire.S01E03.1080p.x264-RARBG",
		"Война и мир 2022",
		"Random Spanish Movie 2024",
	} {
		r := w.Match(name)
		if r.Tier != Tier1 {
			t.Errorf("name=%q: got tier %v, want Tier1 (NoOp must never claim a match)", name, r.Tier)
		}
		if r.SkipReason != "" {
			t.Errorf("name=%q: NoOp returned SkipReason %q (must be empty)", name, r.SkipReason)
		}
	}
}

func TestNoOp_EnabledAndEnforceAreFalse(t *testing.T) {
	w := New(NewDefaultConfig())
	if w.Enabled() {
		t.Errorf("NoOp.Enabled() must be false")
	}
	if w.Enforce() {
		t.Errorf("NoOp.Enforce() must be false")
	}
}

func TestNoOp_SnapshotIsEmpty(t *testing.T) {
	w := New(NewDefaultConfig())
	s := w.Snapshot()
	if s.FingerprintN != 0 {
		t.Errorf("NoOp.Snapshot().FingerprintN: got %d, want 0", s.FingerprintN)
	}
	if len(s.Sources) != 0 {
		t.Errorf("NoOp.Snapshot().Sources: got %d entries, want 0", len(s.Sources))
	}
}

func TestNoOp_RefreshIsNoop(t *testing.T) {
	w := New(NewDefaultConfig())
	if err := w.Refresh(context.Background()); err != nil {
		t.Errorf("NoOp.Refresh: got err %v, want nil", err)
	}
}

func TestNewDefaultConfig_ShipSafe(t *testing.T) {
	// Ensure defaults are safe — the package must be a no-op out
	// of the box. Specifically: Enabled=false, Enforce=false, no
	// *arr URLs/keys.
	c := NewDefaultConfig()
	if c.Enabled {
		t.Errorf("default Enabled must be false (operator opts in)")
	}
	if c.Enforce {
		t.Errorf("default Enforce must be false")
	}
	if c.HasAnySource() {
		t.Errorf("default HasAnySource: got true, want false (no *arr URLs in defaults)")
	}
	if c.PollInterval == "" {
		t.Errorf("default PollInterval should be set so a future enable doesn't crash on empty parse")
	}
	if c.BloomCapacity <= 0 {
		t.Errorf("default BloomCapacity must be positive")
	}
	if c.BloomFalsePositiveRate <= 0 || c.BloomFalsePositiveRate >= 1 {
		t.Errorf("default BloomFalsePositiveRate must be in (0,1)")
	}
	if c.MinTitleTokens <= 0 {
		t.Errorf("default MinTitleTokens must be positive")
	}
}

func TestConfig_HasAnySource(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Config)
		want   bool
	}{
		{"empty defaults", func(c *Config) {}, false},
		{"sonarr only", func(c *Config) { c.SonarrBaseURL = "http://s"; c.SonarrAPIKey = "k" }, true},
		{"radarr only", func(c *Config) { c.RadarrBaseURL = "http://r"; c.RadarrAPIKey = "k" }, true},
		{"lidarr only", func(c *Config) { c.LidarrBaseURL = "http://l"; c.LidarrAPIKey = "k" }, true},
		{"sonarr URL but no key", func(c *Config) { c.SonarrBaseURL = "http://s" }, false},
		{"sonarr key but no URL", func(c *Config) { c.SonarrAPIKey = "k" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewDefaultConfig()
			tt.modify(&c)
			if got := c.HasAnySource(); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTier_String(t *testing.T) {
	tests := []struct {
		t    Tier
		want string
	}{
		{Tier0, "tier0"},
		{Tier1, "tier1"},
		{Tier2, "tier2"},
		{Tier(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.t.String(); got != tt.want {
			t.Errorf("Tier(%d).String(): got %q, want %q", tt.t, got, tt.want)
		}
	}
}
