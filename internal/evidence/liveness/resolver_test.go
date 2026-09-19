package liveness

import (
	"context"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
)

// fakeStore is an in-memory store implementation that captures
// every call so tests can assert on the state machine without
// touching Postgres. It does not attempt to be transactional —
// tests drive single-threaded traffic.
type fakeStore struct {
	rows map[string]*Record

	// Trace of mutating calls in invocation order. Useful for
	// asserting that, e.g., MarkDead happened only once.
	calls []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: make(map[string]*Record)}
}

func key(ih []byte) string { return string(ih) }

func (f *fakeStore) Get(_ context.Context, infoHash []byte) (*Record, error) {
	rec, ok := f.rows[key(infoHash)]
	if !ok {
		return nil, nil
	}
	cp := *rec
	return &cp, nil
}

func (f *fakeStore) MarkAlive(_ context.Context, infoHash []byte, observedAt time.Time, qbState, source string) error {
	f.calls = append(f.calls, "alive")
	f.rows[key(infoHash)] = &Record{
		InfoHash:       append([]byte(nil), infoHash...),
		Status:         StatusAlive,
		LastQBState:    qbState,
		LastObservedAt: observedAt,
		AliveSource:    source,
		UpdatedAt:      observedAt,
	}
	return nil
}

func (f *fakeStore) RecordSuspect(_ context.Context, infoHash []byte, observedAt time.Time, qbState string) (*Record, error) {
	f.calls = append(f.calls, "suspect")
	rec, ok := f.rows[key(infoHash)]
	if !ok {
		first := observedAt
		rec = &Record{
			InfoHash:            append([]byte(nil), infoHash...),
			Status:              StatusSuspect,
			LastQBState:         qbState,
			SuspectFirstSeenAt:  &first,
			SuspectObservations: 1,
			LastObservedAt:      observedAt,
			UpdatedAt:           observedAt,
		}
		f.rows[key(infoHash)] = rec
		cp := *rec
		return &cp, nil
	}
	rec.LastQBState = qbState
	switch rec.Status {
	case StatusDead:
		// Promotion to dead is sticky — RecordSuspect must not
		// reset the count or revive a dead row.
		rec.LastObservedAt = observedAt
		rec.UpdatedAt = observedAt
	case StatusAlive:
		// Alive → suspect transition. Start a fresh stall window.
		first := observedAt
		rec.Status = StatusSuspect
		rec.SuspectFirstSeenAt = &first
		rec.SuspectObservations = 1
		rec.LastObservedAt = observedAt
		rec.UpdatedAt = observedAt
	default:
		if rec.SuspectFirstSeenAt == nil {
			first := observedAt
			rec.SuspectFirstSeenAt = &first
		}
		rec.SuspectObservations++
		rec.LastObservedAt = observedAt
		rec.UpdatedAt = observedAt
	}
	cp := *rec
	return &cp, nil
}

func (f *fakeStore) MarkDead(_ context.Context, infoHash []byte, blacklistedAt time.Time, ttl time.Duration) error {
	f.calls = append(f.calls, "dead")
	rec, ok := f.rows[key(infoHash)]
	if !ok {
		return nil
	}
	rec.Status = StatusDead
	rec.BlacklistedAt = &blacklistedAt
	due := blacklistedAt.Add(ttl)
	rec.NextRevalidateAt = &due
	rec.UpdatedAt = blacklistedAt
	return nil
}

type noopMetrics struct{}

func (noopMetrics) Observation(_, _ string) {}

func newResolverForTest(store store, cfg evidence.LivenessConfig, now time.Time) *Resolver {
	r := NewResolver(store, cfg, noopMetrics{}, nil)
	r.now = func() time.Time { return now }
	return r
}

func defaultCfg() evidence.LivenessConfig {
	cfg := evidence.NewDefaultLivenessConfig()
	cfg.Enabled = true
	return cfg
}

func suspectEv(infoHash []byte, observedAt time.Time, state string) evidence.Evidence {
	return evidence.Evidence{
		Source:     evidence.SourceQBittorrent,
		Kind:       evidence.KindQBStateObservation,
		InfoHash:   infoHash,
		ObservedAt: observedAt,
		Strength:   evidence.StrengthQBStateSuspect,
		QBState:    state,
	}
}

func aliveEv(infoHash []byte, observedAt time.Time, state string) evidence.Evidence {
	return evidence.Evidence{
		Source:     evidence.SourceQBittorrent,
		Kind:       evidence.KindQBStateObservation,
		InfoHash:   infoHash,
		ObservedAt: observedAt,
		Strength:   evidence.StrengthQBStateAlive,
		QBState:    state,
	}
}

// TestSingleSuspectDoesNotFlipDead — a single suspect observation
// must NOT promote the row to dead. Flooding the threshold from a
// single transient stall would bury healthy torrents.
func TestSingleSuspectDoesNotFlipDead(t *testing.T) {
	store := newFakeStore()
	cfg := defaultCfg()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := newResolverForTest(store, cfg, now)

	ih := []byte("aaaaaaaaaaaaaaaaaaaa")
	r.HandleEvidence(context.Background(), suspectEv(ih, now, "stalledDL"))

	rec, _ := store.Get(context.Background(), ih)
	if rec == nil || rec.Status != StatusSuspect {
		t.Fatalf("expected suspect, got %+v", rec)
	}
	for _, c := range store.calls {
		if c == "dead" {
			t.Fatalf("dead transition should not have fired on single observation")
		}
	}
}

// TestSuspectBelowThresholdStaysSuspect — N observations all within
// the stall window keep the row suspect.
func TestSuspectBelowThresholdStaysSuspect(t *testing.T) {
	store := newFakeStore()
	cfg := defaultCfg()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := newResolverForTest(store, cfg, now)

	ih := []byte("bbbbbbbbbbbbbbbbbbbb")
	// 3 observations all at the same wall-clock = same suspect_first_seen_at.
	for i := 0; i < 3; i++ {
		r.HandleEvidence(context.Background(), suspectEv(ih, now, "stalledDL"))
	}

	rec, _ := store.Get(context.Background(), ih)
	if rec == nil || rec.Status != StatusSuspect {
		t.Fatalf("expected suspect, got %+v", rec)
	}
}

// TestSuspectPastThresholdPromotesDead — once enough observations
// have accumulated AND the stall window has been exceeded, the row
// flips to dead.
func TestSuspectPastThresholdPromotesDead(t *testing.T) {
	store := newFakeStore()
	cfg := defaultCfg() // threshold 4h, min observations 2
	first := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// First observation — store as suspect, "now" same as first.
	r := newResolverForTest(store, cfg, first)
	ih := []byte("cccccccccccccccccccc")
	r.HandleEvidence(context.Background(), suspectEv(ih, first, "stalledDL"))

	// Second observation 5 hours later — past threshold, count >= min.
	later := first.Add(5 * time.Hour)
	r.now = func() time.Time { return later }
	r.HandleEvidence(context.Background(), suspectEv(ih, later, "stalledDL"))

	rec, _ := store.Get(context.Background(), ih)
	if rec == nil || rec.Status != StatusDead {
		t.Fatalf("expected dead, got %+v", rec)
	}
	if rec.BlacklistedAt == nil {
		t.Fatal("expected BlacklistedAt set on dead row")
	}
}

// TestAliveAlwaysWins — an alive observation flips a suspect or dead
// row back to alive. The state machine never refuses to recover.
func TestAliveAlwaysWins(t *testing.T) {
	cases := []struct {
		name   string
		seedFn func(s *fakeStore, ih []byte)
	}{
		{
			name: "from suspect",
			seedFn: func(s *fakeStore, ih []byte) {
				first := time.Now().Add(-time.Hour)
				s.rows[key(ih)] = &Record{
					InfoHash:            ih,
					Status:              StatusSuspect,
					SuspectFirstSeenAt:  &first,
					SuspectObservations: 1,
				}
			},
		},
		{
			name: "from dead",
			seedFn: func(s *fakeStore, ih []byte) {
				now := time.Now()
				s.rows[key(ih)] = &Record{
					InfoHash:      ih,
					Status:        StatusDead,
					BlacklistedAt: &now,
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			cfg := defaultCfg()
			now := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
			r := newResolverForTest(store, cfg, now)

			ih := []byte("dddddddddddddddddddd")
			tc.seedFn(store, ih)

			r.HandleEvidence(context.Background(), aliveEv(ih, now, "downloading"))

			rec, _ := store.Get(context.Background(), ih)
			if rec == nil || rec.Status != StatusAlive {
				t.Fatalf("expected alive, got %+v", rec)
			}
			if rec.AliveSource != AliveSourceQBState {
				t.Fatalf("expected alive_source=%s, got %q", AliveSourceQBState, rec.AliveSource)
			}
		})
	}
}

// TestAliveThenSuspectAcrossThresholdPromotesDead — a review finding
// regression test. A previously-alive row that accumulates suspect
// observations spanning the stall window must promote to dead,
// otherwise alive rows could collect stalls forever without ever
// being blacklisted.
func TestAliveThenSuspectAcrossThresholdPromotesDead(t *testing.T) {
	store := newFakeStore()
	cfg := defaultCfg() // threshold 4h, min observations 2
	t0 := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	r := newResolverForTest(store, cfg, t0)

	ih := []byte("jjjjjjjjjjjjjjjjjjjj")

	// Seed alive at t0.
	r.HandleEvidence(context.Background(), aliveEv(ih, t0, "downloading"))

	// First suspect at t0+10m — alive→suspect transition; the
	// stall window must restart from this observation.
	t1 := t0.Add(10 * time.Minute)
	r.now = func() time.Time { return t1 }
	r.HandleEvidence(context.Background(), suspectEv(ih, t1, "stalledDL"))

	if rec, _ := store.Get(context.Background(), ih); rec == nil || rec.Status != StatusSuspect {
		t.Fatalf("expected suspect after alive→suspect, got %+v", rec)
	}

	// Second suspect 5h later — past threshold, count >= min →
	// must flip to dead.
	t2 := t1.Add(5 * time.Hour)
	r.now = func() time.Time { return t2 }
	r.HandleEvidence(context.Background(), suspectEv(ih, t2, "stalledDL"))

	rec, _ := store.Get(context.Background(), ih)
	if rec == nil || rec.Status != StatusDead {
		t.Fatalf("expected dead after alive→suspect→threshold, got %+v", rec)
	}
}

// TestArrImportPrivateCategorySkipped — when the toggle is on (the
// default), an *arr import on a private-tracker grab does NOT mark
// the public catalog hash alive.
func TestArrImportPrivateCategorySkipped(t *testing.T) {
	store := newFakeStore()
	cfg := defaultCfg()
	cfg.ExcludePrivateTrackerGrabs = true
	now := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	r := newResolverForTest(store, cfg, now)

	ih := []byte("eeeeeeeeeeeeeeeeeeee")
	ev := evidence.Evidence{
		Source:     evidence.SourceSonarr,
		Kind:       evidence.KindWebhookImport,
		InfoHash:   ih,
		ObservedAt: now,
		Strength:   evidence.StrengthArrWebhookImport,
		Category:   "private",
	}
	r.HandleEvidence(context.Background(), ev)

	if rec, _ := store.Get(context.Background(), ih); rec != nil {
		t.Fatalf("expected no row written for private import, got %+v", rec)
	}
}

// TestArrImportPrivateCategoryAllowed — when the toggle is off, a
// private-tracker import is treated like any other import and does
// mark the hash alive.
func TestArrImportPrivateCategoryAllowed(t *testing.T) {
	store := newFakeStore()
	cfg := defaultCfg()
	cfg.ExcludePrivateTrackerGrabs = false
	now := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	r := newResolverForTest(store, cfg, now)

	ih := []byte("ffffffffffffffffffff")
	ev := evidence.Evidence{
		Source:     evidence.SourceSonarr,
		Kind:       evidence.KindWebhookImport,
		InfoHash:   ih,
		ObservedAt: now,
		Strength:   evidence.StrengthArrWebhookImport,
		Category:   "private",
	}
	r.HandleEvidence(context.Background(), ev)

	rec, _ := store.Get(context.Background(), ih)
	if rec == nil || rec.Status != StatusAlive {
		t.Fatalf("expected alive row for private import with toggle off, got %+v", rec)
	}
	if rec.AliveSource != AliveSourceArrWebhook {
		t.Fatalf("expected alive_source=%s, got %q", AliveSourceArrWebhook, rec.AliveSource)
	}
}

// TestArrImportPublicCategoryAlwaysAlive — the toggle has no effect
// on non-private categories.
func TestArrImportPublicCategoryAlwaysAlive(t *testing.T) {
	store := newFakeStore()
	cfg := defaultCfg()
	cfg.ExcludePrivateTrackerGrabs = true
	now := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	r := newResolverForTest(store, cfg, now)

	ih := []byte("gggggggggggggggggggg")
	ev := evidence.Evidence{
		Source:     evidence.SourceSonarr,
		Kind:       evidence.KindWebhookImport,
		InfoHash:   ih,
		ObservedAt: now,
		Strength:   evidence.StrengthArrWebhookImport,
		Category:   "public",
	}
	r.HandleEvidence(context.Background(), ev)

	rec, _ := store.Get(context.Background(), ih)
	if rec == nil || rec.Status != StatusAlive {
		t.Fatalf("expected alive row for public import, got %+v", rec)
	}
}

// TestDisabledIsNoOp — when liveness.enabled=false, no row is ever
// written, regardless of evidence kind. This is the safety property
// that guarantees zero behaviour change for deploys that haven't
// opted in.
func TestDisabledIsNoOp(t *testing.T) {
	store := newFakeStore()
	cfg := evidence.NewDefaultLivenessConfig()
	cfg.Enabled = false
	r := newResolverForTest(store, cfg, time.Now())

	ih := []byte("hhhhhhhhhhhhhhhhhhhh")
	r.HandleEvidence(context.Background(), suspectEv(ih, time.Now(), "stalledDL"))
	r.HandleEvidence(context.Background(), aliveEv(ih, time.Now(), "downloading"))

	if rec, _ := store.Get(context.Background(), ih); rec != nil {
		t.Fatalf("disabled resolver wrote a row: %+v", rec)
	}
	if len(store.calls) != 0 {
		t.Fatalf("disabled resolver issued mutating calls: %v", store.calls)
	}
}

// TestArrGrabIsNotAlive — *arr grab webhooks indicate intent, not
// confirmation. The resolver must distinguish grabs from imports
// (they share KindPollHistory in the poller path) by strength.
func TestArrGrabIsNotAlive(t *testing.T) {
	store := newFakeStore()
	cfg := defaultCfg()
	now := time.Now()
	r := newResolverForTest(store, cfg, now)

	ih := []byte("iiiiiiiiiiiiiiiiiiii")
	ev := evidence.Evidence{
		Source:     evidence.SourceRadarr,
		Kind:       evidence.KindPollHistory,
		InfoHash:   ih,
		ObservedAt: now,
		Strength:   evidence.StrengthArrPollGrab,
	}
	r.HandleEvidence(context.Background(), ev)

	if rec, _ := store.Get(context.Background(), ih); rec != nil {
		t.Fatalf("grab should not produce a liveness row, got %+v", rec)
	}
}
