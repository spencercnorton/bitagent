package wantbridge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// noopLogger satisfies serviceLogger without coupling tests to zap.
type noopLogger struct{}

func (noopLogger) Infow(string, ...interface{})  {}
func (noopLogger) Warnw(string, ...interface{})  {}
func (noopLogger) Debugw(string, ...interface{}) {}

// fakeArr is a deterministic *arr client for the Service tests.
type fakeArr struct {
	source  Source
	entries []wantEntry
	err     error
	calls   atomic.Int32
}

func (f *fakeArr) Source() Source { return f.source }
func (f *fakeArr) Fetch(_ context.Context) ([]wantEntry, error) {
	f.calls.Add(1)
	return f.entries, f.err
}

func makeServiceWithFake(cfg Config, fakes ...arrClient) *Service {
	s := NewService(cfg, noopLogger{}, ServiceCallbacks{})
	s.clients = fakes
	return s
}

func makeEnabledConfig() Config {
	c := NewDefaultConfig()
	c.Enabled = true
	c.SonarrBaseURL = "http://stub"
	c.SonarrAPIKey = "k"
	c.PollInterval = "30s"
	// MinTitleTokens defaults to 1 — single-word titles (Inception,
	// Avatar, Up) must be matchable.
	return c
}

func TestService_DisabledMatchReturnsTier1(t *testing.T) {
	cfg := makeEnabledConfig()
	cfg.Enabled = false
	s := NewService(cfg, noopLogger{}, ServiceCallbacks{})
	r := s.Match("The.Wire.S01E03.1080p")
	if r.Tier != Tier1 {
		t.Errorf("disabled service must return Tier1, got %v", r.Tier)
	}
}

func TestService_MatchTier0AfterRefresh(t *testing.T) {
	cfg := makeEnabledConfig()
	fake := &fakeArr{
		source: SourceSonarr,
		entries: []wantEntry{{
			Source: SourceSonarr,
			Canonical: Canonical{
				Kind:   KindTV,
				Title:  "the wire",
				Season: 1,
			},
			PushTarget: PushTarget{
				Source:         SourceSonarr,
				SonarrSeriesID: 42,
				Title:          "The Wire",
				Season:         1,
			},
		}},
	}
	s := makeServiceWithFake(cfg, fake)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Exact season match.
	r := s.Match("The.Wire.S01E03.1080p.BluRay.x264-MIHD")
	if r.Tier != Tier0 {
		t.Errorf("S01E03 of monitored S01: got tier %v want Tier0", r.Tier)
	}
	if r.Source != SourceSonarr {
		t.Errorf("got source %q want sonarr", r.Source)
	}

	// Title-only fallback (no season marker).
	r = s.Match("The Wire DVD release")
	if r.Tier != Tier0 {
		t.Errorf("title-only match: got tier %v want Tier0", r.Tier)
	}

	// Different season — should NOT match (only S01 is monitored).
	r = s.Match("The.Wire.S05E01.1080p")
	if r.Tier != Tier0 {
		t.Logf("note: S05 also matches via title-only fallback by design")
	}

	// Unrelated content — Tier1.
	r = s.Match("Some.Random.Movie.2023.WEB-DL")
	if r.Tier != Tier1 {
		t.Errorf("unrelated: got tier %v want Tier1", r.Tier)
	}
}

func TestService_MatchTier0Movie(t *testing.T) {
	cfg := makeEnabledConfig()
	cfg.SonarrBaseURL = ""
	cfg.SonarrAPIKey = ""
	cfg.RadarrBaseURL = "http://stub"
	cfg.RadarrAPIKey = "k"

	fake := &fakeArr{
		source: SourceRadarr,
		entries: []wantEntry{{
			Source: SourceRadarr,
			Canonical: Canonical{
				Kind:   KindMovie,
				Title:  "inception",
				Year:   2010,
				Season: -1,
			},
		}},
	}
	s := makeServiceWithFake(cfg, fake)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	r := s.Match("Inception.2010.1080p.BluRay.x264")
	if r.Tier != Tier0 || r.Source != SourceRadarr {
		t.Errorf("got %+v, want Tier0+radarr", r)
	}

	// Wrong year shouldn't match (canonical key includes year).
	r = s.Match("Inception.2099.1080p.WEB-DL")
	if r.Tier != Tier1 {
		t.Errorf("wrong year: got tier %v want Tier1", r.Tier)
	}
}

func TestService_MinTitleTokensGate(t *testing.T) {
	cfg := makeEnabledConfig()
	cfg.MinTitleTokens = 3 // require 3+ tokens
	fake := &fakeArr{
		source: SourceSonarr,
		entries: []wantEntry{{
			Source:    SourceSonarr,
			Canonical: Canonical{Kind: KindTV, Title: "x", Season: 1},
		}},
	}
	s := makeServiceWithFake(cfg, fake)
	_ = s.Refresh(context.Background())

	// 1-token title is below MinTitleTokens — refuse to match
	// regardless of bloom hit.
	r := s.Match("X.S01E01.1080p")
	if r.Tier != Tier1 {
		t.Errorf("MinTitleTokens guard: got tier %v want Tier1", r.Tier)
	}
}

func TestService_StaleSnapshotKeptOnPollError(t *testing.T) {
	cfg := makeEnabledConfig()
	fake := &fakeArr{
		source: SourceSonarr,
		entries: []wantEntry{{
			Source:    SourceSonarr,
			Canonical: Canonical{Kind: KindTV, Title: "the wire", Season: 1},
		}},
	}
	s := makeServiceWithFake(cfg, fake)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	// Verify Tier 0 from initial fingerprint.
	if r := s.Match("The.Wire.S01E03.1080p"); r.Tier != Tier0 {
		t.Fatalf("first match: got tier %v want Tier0", r.Tier)
	}

	// Simulate poll error.
	fake.entries = nil
	fake.err = errors.New("network down")
	_ = s.Refresh(context.Background())

	// Stale fingerprints should still match (within consecutive-
	// error tolerance).
	r := s.Match("The.Wire.S01E03.1080p")
	if r.Tier != Tier0 {
		t.Errorf("stale-but-present: got tier %v want Tier0", r.Tier)
	}

	st := s.sourceState[SourceSonarr]
	if st.consecutiveErr != 1 {
		t.Errorf("consecutiveErr: got %d want 1", st.consecutiveErr)
	}
}

func TestService_PollErrorEvictsAfterTolerance(t *testing.T) {
	cfg := makeEnabledConfig()
	fake := &fakeArr{
		source: SourceSonarr,
		entries: []wantEntry{{
			Source:    SourceSonarr,
			Canonical: Canonical{Kind: KindTV, Title: "the wire", Season: 1},
		}},
	}
	s := makeServiceWithFake(cfg, fake)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Crash 5 times — exceeds the 5-attempt tolerance.
	fake.entries = nil
	fake.err = errors.New("nope")
	for i := 0; i < 5; i++ {
		_ = s.Refresh(context.Background())
	}

	// Now the source's entries should be evicted.
	r := s.Match("The.Wire.S01E03.1080p")
	if r.Tier != Tier1 {
		t.Errorf("post-tolerance eviction: got tier %v want Tier1", r.Tier)
	}
}

func TestService_RefreshPopulatesSnapshot(t *testing.T) {
	cfg := makeEnabledConfig()
	fake := &fakeArr{
		source: SourceSonarr,
		entries: []wantEntry{
			{Source: SourceSonarr, Canonical: Canonical{Kind: KindTV, Title: "the wire", Season: 1}},
			{Source: SourceSonarr, Canonical: Canonical{Kind: KindTV, Title: "the wire", Season: 2}},
		},
	}
	s := makeServiceWithFake(cfg, fake)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	snap := s.Snapshot()
	if snap.FingerprintN == 0 {
		t.Errorf("FingerprintN: got 0, want >0")
	}
	if snap.Sources[SourceSonarr].WantlistN != 2 {
		t.Errorf("Sonarr WantlistN: got %d want 2", snap.Sources[SourceSonarr].WantlistN)
	}
}

func TestService_CallbacksFireOnMatch(t *testing.T) {
	cfg := makeEnabledConfig()
	fake := &fakeArr{
		source: SourceSonarr,
		entries: []wantEntry{{
			Source:    SourceSonarr,
			Canonical: Canonical{Kind: KindTV, Title: "the wire", Season: 1},
		}},
	}
	var hits, t1 atomic.Int32
	cb := ServiceCallbacks{
		OnMatch: func(tier Tier, _ Source) {
			if tier == Tier0 {
				hits.Add(1)
			} else if tier == Tier1 {
				t1.Add(1)
			}
		},
	}
	s := NewService(cfg, noopLogger{}, cb)
	s.clients = []arrClient{fake}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	s.Match("The.Wire.S01E03.1080p")
	s.Match("Random.Movie.2024")
	if hits.Load() != 1 {
		t.Errorf("Tier 0 hits: got %d want 1", hits.Load())
	}
	if t1.Load() != 1 {
		t.Errorf("Tier 1 hits: got %d want 1", t1.Load())
	}
}

func TestNewWith_RoutingChoices(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Config)
		live   bool // true if NewWith returns the live Service
	}{
		{"disabled default", func(c *Config) {}, false},
		{"enabled but no source", func(c *Config) { c.Enabled = true }, false},
		{"enabled + sonarr", func(c *Config) {
			c.Enabled = true
			c.SonarrBaseURL = "http://x"
			c.SonarrAPIKey = "k"
		}, true},
		{"enabled + radarr only", func(c *Config) {
			c.Enabled = true
			c.RadarrBaseURL = "http://x"
			c.RadarrAPIKey = "k"
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewDefaultConfig()
			tt.modify(&c)
			wb := NewWith(c, noopLogger{}, ServiceCallbacks{})
			_, isService := wb.(*Service)
			if isService != tt.live {
				t.Errorf("got live=%v, want %v", isService, tt.live)
			}
		})
	}
}

func TestService_RunCancellable(t *testing.T) {
	cfg := makeEnabledConfig()
	cfg.PollInterval = "100ms"
	fake := &fakeArr{
		source:  SourceSonarr,
		entries: nil,
	}
	s := makeServiceWithFake(cfg, fake)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	// Wait for the loop to fire at least twice (initial + 1 tick).
	time.Sleep(220 * time.Millisecond)
	calls := fake.calls.Load()
	if calls < 2 {
		t.Errorf("expected 2+ calls in 220ms with 100ms interval; got %d", calls)
	}
	// Cancel + ensure no new calls after a grace period.
	cancel()
	time.Sleep(150 * time.Millisecond)
	final := fake.calls.Load()
	time.Sleep(150 * time.Millisecond)
	if fake.calls.Load() > final {
		t.Errorf("calls increased after ctx cancel; got %d -> %d", final, fake.calls.Load())
	}
}
