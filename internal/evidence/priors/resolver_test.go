package priors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
)

// fakeStore is the in-memory test double for store. It records
// every call so individual tests can make targeted assertions
// without standing up Postgres.
type fakeStore struct {
	grabs       []GrabAttempt
	pending     map[string][]GrabAttempt
	successKeys []FeatureKey
	failureKeys []FeatureKey

	deletedPrivate int

	failGrab    error
	failResolve error
	failApply   error
	failDelete  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		pending: make(map[string][]GrabAttempt),
	}
}

func (f *fakeStore) RecordGrab(_ context.Context, ev GrabAttempt) (int64, error) {
	if f.failGrab != nil {
		return 0, f.failGrab
	}
	id := int64(len(f.grabs) + 1)
	ev.ID = id
	f.grabs = append(f.grabs, ev)
	key := string(ev.InfoHash)
	f.pending[key] = append(f.pending[key], ev)
	return id, nil
}

// resolvePending is shared between the legacy and atomic resolution
// methods so the in-memory state machine stays consistent.
func (f *fakeStore) resolvePending(infoHash []byte, outcome Outcome, resolvedAt time.Time) []GrabAttempt {
	key := string(infoHash)
	pend := f.pending[key]
	delete(f.pending, key)
	out := make([]GrabAttempt, len(pend))
	for i, p := range pend {
		p.ResolvedAt = &resolvedAt
		p.Outcome = outcome
		out[i] = p
	}
	return out
}

func (f *fakeStore) ResolvePendingAndIncrement(
	_ context.Context, infoHash []byte, outcome Outcome, resolvedAt time.Time,
) ([]GrabAttempt, []FeatureKey, error) {
	if f.failResolve != nil {
		return nil, nil, f.failResolve
	}
	resolved := f.resolvePending(infoHash, outcome, resolvedAt)
	if len(resolved) == 0 {
		return nil, nil, nil
	}
	keys := mergeFeatures(resolved)
	if len(keys) == 0 {
		return resolved, nil, nil
	}
	if f.failApply != nil {
		// Simulate the atomic guarantee: if priors fail, the
		// resolution rolls back too.
		for _, p := range resolved {
			p.ResolvedAt = nil
			p.Outcome = ""
			f.pending[string(p.InfoHash)] = append(f.pending[string(p.InfoHash)], p)
		}
		return nil, nil, f.failApply
	}
	switch outcome {
	case OutcomeSuccess:
		f.successKeys = append(f.successKeys, keys...)
	case OutcomeFailure:
		f.failureKeys = append(f.failureKeys, keys...)
	}
	return resolved, keys, nil
}

func (f *fakeStore) DeletePendingByInfoHash(_ context.Context, infoHash []byte) (int64, error) {
	if f.failDelete != nil {
		return 0, f.failDelete
	}
	key := string(infoHash)
	n := int64(len(f.pending[key]))
	delete(f.pending, key)
	if n > 0 {
		f.deletedPrivate += int(n)
	}
	return n, nil
}

func (f *fakeStore) IncrementPriors(_ context.Context, success, failure []FeatureKey) error {
	if f.failApply != nil {
		return f.failApply
	}
	f.successKeys = append(f.successKeys, success...)
	f.failureKeys = append(f.failureKeys, failure...)
	return nil
}

// fakeMetrics records observation events; tests assert on tag pairs
// without spinning up a Prometheus registry.
type fakeMetrics struct {
	events   []string
	outcomes []string
}

func (f *fakeMetrics) Observation(event, outcome string) {
	f.events = append(f.events, event+":"+outcome)
}

func (f *fakeMetrics) Outcome(class string) { f.outcomes = append(f.outcomes, class) }

// fakeSourceLookup returns a fixed source list and extension
// regardless of the infohash.
type fakeSourceLookup struct {
	sources []string
	ext     string
	err     error
}

func (l fakeSourceLookup) SourcesForInfoHash(_ context.Context, _ []byte) ([]string, string, error) {
	return l.sources, l.ext, l.err
}

func enabledCfg() evidence.OutcomePriorsConfig {
	c := evidence.NewDefaultOutcomePriorsConfig()
	c.Enabled = true
	c.Apply = true
	return c
}

func makeResolver(t *testing.T, store store, sources SourceLookup) (*Resolver, *fakeMetrics) {
	t.Helper()
	m := &fakeMetrics{}
	r := NewResolver(store, sources, enabledCfg(), m, nil)
	return r, m
}

func TestHandleEvidence_DisabledIsNoop(t *testing.T) {
	store := newFakeStore()
	cfg := evidence.NewDefaultOutcomePriorsConfig()
	r := NewResolver(store, nil, cfg, &fakeMetrics{}, nil)
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookGrab,
		InfoHash: []byte{0x01},
		Title:    "Foo.2024.1080p-NTb",
	})
	if len(store.grabs) != 0 {
		t.Fatalf("disabled resolver wrote a grab: %+v", store.grabs)
	}
}

func TestHandleGrab_RecordsAttemptAndFeatures(t *testing.T) {
	store := newFakeStore()
	r, metrics := makeResolver(t, store, fakeSourceLookup{
		sources: []string{"http://tracker.foo.com/announce"},
		ext:     "mkv",
	})
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookGrab,
		InfoHash: []byte{0xAA, 0xBB},
		Title:    "Test.Show.S01E01.1080p.WEB-DL.x265-NTb",
		Source:   evidence.SourceSonarr,
	})
	if len(store.grabs) != 1 {
		t.Fatalf("expected 1 grab recorded, got %d", len(store.grabs))
	}
	got := store.grabs[0]
	wantFeatures := map[FeatureKey]bool{
		{Type: FeatureSource, Value: "foo.com"}:   true,
		{Type: FeatureReleaseGroup, Value: "ntb"}: true,
		{Type: FeatureQualityTag, Value: "webdl"}: true,
		{Type: FeatureResolution, Value: "1080p"}: true,
		{Type: FeatureCodec, Value: "x265"}:       true,
		{Type: FeatureExtension, Value: "mkv"}:    true,
	}
	for _, f := range got.Features {
		if !wantFeatures[f] {
			t.Errorf("unexpected feature %+v", f)
		}
		delete(wantFeatures, f)
	}
	if len(wantFeatures) != 0 {
		t.Errorf("missing features: %+v", wantFeatures)
	}
	mustContain(t, metrics.events, "grab_recorded:ok")
}

func TestHandleImport_ResolvesPendingAndAppliesAlpha(t *testing.T) {
	store := newFakeStore()
	r, metrics := makeResolver(t, store, fakeSourceLookup{
		sources: []string{"udp://tracker.bar.org:6969"},
		ext:     "mkv",
	})
	hash := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookGrab,
		InfoHash: hash,
		Title:    "Foo.2024.1080p.WEB-DL.x264-RARBG",
		Source:   evidence.SourceRadarr,
	})
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookImport,
		InfoHash: hash,
	})
	if got := len(store.successKeys); got == 0 {
		t.Fatalf("expected success keys after import; got %d", got)
	}
	if got := len(store.failureKeys); got != 0 {
		t.Fatalf("import must not write failure keys; got %d", got)
	}
	mustContain(t, metrics.events, "import_resolved:ok")
	mustContain(t, metrics.outcomes, "success")
}

func TestHandleImport_NoPendingIsCountedNotFailed(t *testing.T) {
	store := newFakeStore()
	r, metrics := makeResolver(t, store, fakeSourceLookup{})
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookImport,
		InfoHash: []byte{0xCA, 0xFE},
	})
	mustContain(t, metrics.events, "import_resolved:no_pending")
	if len(store.successKeys) != 0 {
		t.Fatalf("expected no priors update without a pending grab; got %d keys", len(store.successKeys))
	}
}

func TestHandleImport_PrivateCategoryIsSkipped(t *testing.T) {
	store := newFakeStore()
	r, metrics := makeResolver(t, store, fakeSourceLookup{})
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookImport,
		InfoHash: []byte{0xCA, 0xFE},
		Category: "private",
	})
	mustContain(t, metrics.events, "import_resolved:skip_private")
	if len(store.successKeys) != 0 {
		t.Fatalf("private grab should not move alpha; got %d keys", len(store.successKeys))
	}
}

// TestHandleImport_PrivateImportDropsPendingGrab is the regression
// for the AI Finding (high) — when a Grab webhook recorded a pending
// row and the matching Import arrives flagged as private, the
// pending row must be deleted (not left for the expirer to mark
// failure) and neither α nor β must move.
func TestHandleImport_PrivateImportDropsPendingGrab(t *testing.T) {
	store := newFakeStore()
	r, metrics := makeResolver(t, store, fakeSourceLookup{
		sources: []string{"udp://tracker.bar.org:6969"},
		ext:     "mkv",
	})
	hash := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	// 1. Public-style Grab event arrives first → pending row stored.
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookGrab,
		InfoHash: hash,
		Title:    "Foo.2024.1080p.WEB-DL.x264-RARBG",
		Source:   evidence.SourceRadarr,
	})
	if got := len(store.pending[string(hash)]); got != 1 {
		t.Fatalf("expected 1 pending row after Grab; got %d", got)
	}

	// 2. Import comes back tagged private — must drop the pending row
	//    rather than leaving it for the expirer.
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookImport,
		InfoHash: hash,
		Category: "private",
	})

	if got := len(store.pending[string(hash)]); got != 0 {
		t.Fatalf("expected pending row dropped on private import; got %d", got)
	}
	if got := store.deletedPrivate; got != 1 {
		t.Fatalf("expected 1 pending row deleted; got %d", got)
	}
	if len(store.successKeys) != 0 || len(store.failureKeys) != 0 {
		t.Fatalf("private import must not move priors: alpha=%d beta=%d",
			len(store.successKeys), len(store.failureKeys))
	}
	mustContain(t, metrics.events, "import_resolved:skip_private_dropped")

	// 3. Simulate the expirer running over what's left — there is
	//    nothing left to expire, so β must stay zero.
	r.ApplyExpired(context.Background(), nil)
	if len(store.failureKeys) != 0 {
		t.Fatalf("expirer must not produce failure keys for a private-skipped grab")
	}
}

// TestHandleImport_AtomicRollbackOnPriorsFailure asserts the
// AI Finding (medium) fix at resolver.go:137 — if the priors update
// fails inside ResolvePendingAndIncrement, the resolution rolls back
// so the next import (or operator-driven retry) can recover.
func TestHandleImport_AtomicRollbackOnPriorsFailure(t *testing.T) {
	store := newFakeStore()
	store.failApply = errors.New("simulated priors update failure")
	r, metrics := makeResolver(t, store, fakeSourceLookup{
		sources: []string{"udp://tracker.bar.org:6969"},
		ext:     "mkv",
	})
	hash := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookGrab,
		InfoHash: hash,
		Title:    "Foo.2024.1080p.WEB-DL.x264-RARBG",
		Source:   evidence.SourceRadarr,
	})

	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookImport,
		InfoHash: hash,
	})

	// The failed import must NOT have applied any α priors.
	if len(store.successKeys) != 0 {
		t.Fatalf("failed atomic resolve+increment must not write priors; got %d keys", len(store.successKeys))
	}
	// The pending row must still be pending so the next event can
	// retry — the rollback property.
	if got := len(store.pending[string(hash)]); got != 1 {
		t.Fatalf("rollback must restore the pending row; got %d", got)
	}
	mustContain(t, metrics.events, "import_resolved:error")
}

func TestApplyExpired_IncrementsFailureKeys(t *testing.T) {
	store := newFakeStore()
	r, metrics := makeResolver(t, store, fakeSourceLookup{})
	r.ApplyExpired(context.Background(), []GrabAttempt{
		{
			InfoHash: []byte{0x01},
			Features: []FeatureKey{
				{Type: FeatureSource, Value: "tracker.example.com"},
				{Type: FeatureCodec, Value: "x265"},
			},
		},
		{
			InfoHash: []byte{0x02},
			Features: []FeatureKey{
				{Type: FeatureSource, Value: "tracker.example.com"},
			},
		},
	})
	if got := len(store.failureKeys); got != 2 {
		t.Fatalf("expected 2 deduped failure keys, got %d (%+v)", got, store.failureKeys)
	}
	mustContain(t, metrics.events, "expirer_resolved:ok")
	if got := len(metrics.outcomes); got != 2 {
		t.Errorf("expected 2 failure outcomes, got %d", got)
	}
}

func TestHandleGrab_StoreErrorIsCountedNotPropagated(t *testing.T) {
	store := newFakeStore()
	store.failGrab = errors.New("simulated db error")
	r, metrics := makeResolver(t, store, fakeSourceLookup{})
	// HandleEvidence returns no error — the resolver swallows on
	// purpose. We assert via metrics.
	r.HandleEvidence(context.Background(), evidence.Evidence{
		Kind:     evidence.KindWebhookGrab,
		InfoHash: []byte{0xAA},
		Title:    "Foo",
	})
	mustContain(t, metrics.events, "grab_recorded:error")
}

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*evidence.OutcomePriorsConfig)
		ok   bool
	}{
		{"defaults disabled", func(c *evidence.OutcomePriorsConfig) {}, true},
		{"valid enabled", func(c *evidence.OutcomePriorsConfig) {
			c.Enabled = true
		}, true},
		{"interval > window", func(c *evidence.OutcomePriorsConfig) {
			c.Enabled = true
			c.ExpirerInterval = 49 * time.Hour
		}, false},
		{"zero window", func(c *evidence.OutcomePriorsConfig) {
			c.Enabled = true
			c.ResolutionWindow = 0
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := evidence.NewDefaultOutcomePriorsConfig()
			c.mut(&cfg)
			err := validateConfig(cfg)
			if c.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !c.ok && err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func mustContain(t *testing.T, hay []string, needle string) {
	t.Helper()
	for _, s := range hay {
		if s == needle {
			return
		}
	}
	t.Errorf("expected to see %q in %v", needle, hay)
}
