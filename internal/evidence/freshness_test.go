package evidence

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestFreshnessStale is the regression guard for the 2026-07-15 dark-feed
// outage: a tracked source that stops succeeding must become stale, and a
// source that has never succeeded at all must not read as healthy forever.
func TestFreshnessStale(t *testing.T) {
	f := NewFreshness()
	if f.Active() {
		t.Fatal("empty tracker should be inactive")
	}

	f.Track(SourceQBittorrent, "qb-alpha", time.Hour)
	f.Track(SourceSonarr, "sonarr", time.Hour)
	if !f.Active() {
		t.Fatal("tracker with instances should be active")
	}

	if stale := f.Stale(time.Now()); len(stale) != 0 {
		t.Fatalf("freshly tracked sources should not be stale, got %v", stale)
	}

	// Age both past the window: a source that has never reported a
	// success since Track seeded it must not read as healthy forever.
	f.set(freshnessKey(SourceQBittorrent, "qb-alpha"), time.Now().Add(-2*time.Hour))
	f.set(freshnessKey(SourceSonarr, "sonarr"), time.Now().Add(-2*time.Hour))
	if stale := f.Stale(time.Now()); len(stale) != 2 {
		t.Fatalf("want both sources stale, got %v", stale)
	}

	// Sonarr keeps polling, qB does not — only qB is reported. This is
	// the exact production shape: healthy *arr feed, dark qB feed.
	f.MarkSuccess(SourceSonarr, "sonarr")
	stale := f.Stale(time.Now())
	if len(stale) != 1 || !strings.HasPrefix(stale[0], "qbittorrent/qb-alpha") {
		t.Fatalf("want only qbittorrent/qb-alpha stale, got %v", stale)
	}
}

// TestNewFreshnessCheck verifies the health check flips down on a stale
// source and stays inactive when nothing is configured.
func TestNewFreshnessCheck(t *testing.T) {
	f := NewFreshness()
	check := NewFreshnessCheck(f)

	if check.IsActive() {
		t.Fatal("check should be inactive with no sources configured")
	}

	f.Track(SourceQBittorrent, "qb-alpha", time.Hour)
	if !check.IsActive() {
		t.Fatal("check should be active once a source is tracked")
	}
	if err := check.Check(context.Background()); err != nil {
		t.Fatalf("fresh source should pass: %v", err)
	}

	// Rewind the recorded success past the window.
	f.set(freshnessKey(SourceQBittorrent, "qb-alpha"), time.Now().Add(-2*time.Hour))
	err := check.Check(context.Background())
	if err == nil {
		t.Fatal("stale source should fail the health check")
	}
	if !strings.Contains(err.Error(), "qbittorrent/qb-alpha") {
		t.Errorf("error should name the stale instance, got %q", err)
	}
}

// TestFreshnessWindowIsPerInstance is the regression guard for the review
// finding that a slow poller bought a fast one extra grace: a 15m qB feed held
// to a 1h *arr feed's window would read healthy for three hours after going
// dark instead of 45 minutes.
func TestFreshnessWindowIsPerInstance(t *testing.T) {
	f := NewFreshness()
	f.Track(SourceQBittorrent, "qb-alpha", FreshnessWindow(15*time.Minute))
	f.Track(SourceSonarr, "sonarr", FreshnessWindow(time.Hour))

	// One hour dark: past qB's 45m window, inside sonarr's 3h window.
	dark := time.Now().Add(-time.Hour)
	f.set(freshnessKey(SourceQBittorrent, "qb-alpha"), dark)
	f.set(freshnessKey(SourceSonarr, "sonarr"), dark)

	stale := f.Stale(time.Now())
	if len(stale) != 1 || !strings.HasPrefix(stale[0], "qbittorrent/qb-alpha") {
		t.Fatalf("want only the fast qB feed stale at 1h, got %v", stale)
	}
}

// TestFreshnessWindowDefaults keeps a non-positive interval from disabling the
// check entirely — a zero window would make Stale skip the instance forever.
func TestFreshnessWindowDefaults(t *testing.T) {
	if got := FreshnessWindow(0); got != 45*time.Minute {
		t.Fatalf("zero interval should fall back to 3x15m, got %s", got)
	}
	if got := FreshnessWindow(-time.Second); got != 45*time.Minute {
		t.Fatalf("negative interval should fall back to 3x15m, got %s", got)
	}
}
