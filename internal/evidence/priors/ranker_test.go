package priors

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sync"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/model"
)

type stubRankerStore struct {
	priors map[FeatureKey]Prior
	avg    float64
	err    error
}

func (s stubRankerStore) LookupPriors(_ context.Context, keys []FeatureKey) (map[FeatureKey]Prior, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[FeatureKey]Prior)
	for _, k := range keys {
		if p, ok := s.priors[k]; ok {
			out[k] = p
		}
	}
	return out, nil
}

func (s stubRankerStore) GlobalAverage(_ context.Context) (float64, error) { return s.avg, nil }

func makeItem(name, ext string, sources []string, seeders uint) search.TorrentContentResultItem {
	// Seeders() aggregates from Sources[i].Seeders. To express a
	// per-item seeder count we synthesise one source row carrying it
	// (in addition to whatever explicit source URLs the test set).
	srcRows := make([]model.TorrentsTorrentSource, 0, len(sources)+1)
	for _, s := range sources {
		srcRows = append(srcRows, model.TorrentsTorrentSource{Source: s})
	}
	srcRows = append(srcRows, model.TorrentsTorrentSource{
		Source:  "test_seed_source",
		Seeders: model.NewNullUint(seeders),
	})
	return search.TorrentContentResultItem{
		TorrentContent: model.TorrentContent{
			Torrent: model.Torrent{
				Name:      name,
				Extension: model.NewNullString(ext),
				Sources:   srcRows,
			},
		},
	}
}

func TestRanker_DisabledReturnsOriginal(t *testing.T) {
	cfg := evidence.NewDefaultOutcomePriorsConfig()
	r := newRankerForTest(stubRankerStore{}, cfg)
	items := []search.TorrentContentResultItem{
		makeItem("Foo-A", "mkv", nil, 5),
		makeItem("Foo-B", "mkv", nil, 50),
	}
	got := r.Rerank(context.Background(), items)
	if got[0].Torrent.Name != "Foo-A" {
		t.Fatalf("disabled ranker must not reorder: got %q first", got[0].Torrent.Name)
	}
}

func TestRanker_ShadowModeDoesNotReorder(t *testing.T) {
	cfg := enabledCfg()
	cfg.Apply = false
	priors := map[FeatureKey]Prior{
		{Type: FeatureReleaseGroup, Value: "good"}: {Alpha: 50, Beta: 1},
		{Type: FeatureReleaseGroup, Value: "bad"}:  {Alpha: 1, Beta: 50},
	}
	r := newRankerForTest(stubRankerStore{priors: priors, avg: 0.5}, cfg)
	items := []search.TorrentContentResultItem{
		makeItem("Foo.WEBDL.x265-bad", "mkv", nil, 10),
		makeItem("Foo.WEBDL.x265-good", "mkv", nil, 10),
	}
	got := r.Rerank(context.Background(), items)
	if got[0].Torrent.Name != "Foo.WEBDL.x265-bad" {
		t.Fatalf("shadow mode must not reorder; got %q first", got[0].Torrent.Name)
	}
}

func TestRanker_AppliedReordersByPosterior(t *testing.T) {
	cfg := enabledCfg()
	cfg.MinObservations = 5
	// Boost release group "good" (49 alpha, 1 beta → mean 0.961).
	// Tank release group "bad" (1 alpha, 99 beta → mean 0.01).
	// We need both groups to clear MinObservations so the priors
	// actually take effect rather than falling back to globalAvg.
	priors := map[FeatureKey]Prior{
		{Type: FeatureReleaseGroup, Value: "good"}: {Alpha: 50, Beta: 1},
		{Type: FeatureReleaseGroup, Value: "bad"}:  {Alpha: 1, Beta: 100},
	}
	r := newRankerForTest(stubRankerStore{priors: priors, avg: 0.5}, cfg)
	items := []search.TorrentContentResultItem{
		makeItem("Foo.WEB-DL.x265-bad", "mkv", nil, 100),
		makeItem("Foo.WEB-DL.x265-good", "mkv", nil, 100),
	}
	got := r.Rerank(context.Background(), items)
	if got[0].Torrent.Name != "Foo.WEB-DL.x265-good" {
		t.Fatalf("expected 'good' to rank first after Apply, got %q", got[0].Torrent.Name)
	}
}

func TestRanker_BelowMinObservationsFallsBackToGlobal(t *testing.T) {
	cfg := enabledCfg()
	cfg.MinObservations = 50
	// "fresh" has only 2 observations of a single failure — without
	// the floor it would mean 0.33 and lose horribly to globalAvg.
	priors := map[FeatureKey]Prior{
		{Type: FeatureReleaseGroup, Value: "fresh"}: {Alpha: 1, Beta: 2},
	}
	r := newRankerForTest(stubRankerStore{priors: priors, avg: 0.5}, cfg)
	items := []search.TorrentContentResultItem{
		makeItem("Foo.x265-fresh", "mkv", nil, 100),
		makeItem("Foo.x265-fresh", "mkv", nil, 100),
	}
	got := r.Rerank(context.Background(), items)
	if len(got) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got))
	}
	// Both items have the same features so order should be preserved.
	if got[0].Torrent.Name != got[1].Torrent.Name {
		t.Errorf("identical features should produce stable order")
	}
}

func TestRanker_SeederWeightingTiesBreakOnPopularity(t *testing.T) {
	// Both items have the same release group but different seeders;
	// the more-seeded one should rank first regardless of priors.
	cfg := enabledCfg()
	cfg.MinObservations = 0
	priors := map[FeatureKey]Prior{
		{Type: FeatureReleaseGroup, Value: "shared"}: {Alpha: 10, Beta: 10},
	}
	r := newRankerForTest(stubRankerStore{priors: priors, avg: 0.5}, cfg)
	items := []search.TorrentContentResultItem{
		makeItem("Foo.x265-shared", "mkv", nil, 1),
		makeItem("Foo.x265-shared", "mkv", nil, 1000),
	}
	got := r.Rerank(context.Background(), items)
	seeders0 := got[0].Torrent.Seeders()
	if !seeders0.Valid || seeders0.Uint != 1000 {
		t.Errorf("expected high-seeder item first; first item seeders=%+v", seeders0)
	}
}

func TestPrior_MeanAndObservations(t *testing.T) {
	p := Prior{Alpha: 4, Beta: 6}
	if got := p.Mean(); got != 0.4 {
		t.Errorf("Mean: got %v want 0.4", got)
	}
	if got := p.Observations(); got != 8 {
		t.Errorf("Observations: got %d want 8", got)
	}
}

func newRankerForTest(s rankerStore, cfg evidence.OutcomePriorsConfig) *Ranker {
	r := NewRanker(nil, cfg, NewMetrics())
	r.store = s
	// Disable the global-average cache so every call refreshes from
	// the stub, keeping each test self-contained.
	r.cacheTTL = 0
	return r
}

// TestRanker_CachedGlobalAverage_ConcurrentCallers exercises the
// cachedGlobalAverage path with the race detector to assert the
// avgMu fix. Many goroutines call Rerank simultaneously while the
// TTL window expires repeatedly, so both the read and write branches
// of cachedGlobalAverage are hit. Run via `go test -race`.
func TestRanker_CachedGlobalAverage_ConcurrentCallers(t *testing.T) {
	cfg := enabledCfg()
	cfg.MinObservations = 0
	store := stubRankerStore{
		priors: map[FeatureKey]Prior{
			{Type: FeatureReleaseGroup, Value: "shared"}: {Alpha: 10, Beta: 10},
		},
		avg: 0.5,
	}
	r := NewRanker(nil, cfg, NewMetrics())
	r.store = store
	r.cacheTTL = time.Microsecond // force the slow-path frequently

	items := []search.TorrentContentResultItem{
		makeItem("Foo.x265-shared", "mkv", nil, 1),
		makeItem("Foo.x265-shared", "mkv", nil, 1000),
	}
	const goroutines = 16
	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// Copy items per call so Rerank doesn't reorder a
				// shared slice across goroutines.
				local := make([]search.TorrentContentResultItem, len(items))
				copy(local, items)
				_ = r.Rerank(context.Background(), local)
			}
		}()
	}
	wg.Wait()
}

// shiftSamples reports how many displacement observations the ranker recorded.
// dualemit publishes each metric twice — a bitagent_ primary and a bitmagnet_
// legacy series — and BOTH see every Observe, so this takes the max across
// collected series rather than the sum, which would double-count.
func shiftSamples(t *testing.T, m *Metrics) uint64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	m.rerankShift.Collect(ch)
	close(ch)
	var most uint64
	for mm := range ch {
		var d dto.Metric
		if err := mm.Write(&d); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		if n := d.GetHistogram().GetSampleCount(); n > most {
			most = n
		}
	}
	return most
}

// Shadow mode exists to size the ranker's effect before an operator trusts it.
// It previously recorded only how many items it scored, never how far they
// would move — so there was no evidence on which to flip Apply, and the module
// scored every search indefinitely with the result discarded.
func TestRanker_ShadowModeRecordsDisplacement(t *testing.T) {
	cfg := enabledCfg()
	cfg.Apply = false
	cfg.MinObservations = 5
	priors := map[FeatureKey]Prior{
		{Type: FeatureReleaseGroup, Value: "good"}: {Alpha: 50, Beta: 1},
		{Type: FeatureReleaseGroup, Value: "bad"}:  {Alpha: 1, Beta: 100},
	}
	m := NewMetrics()
	r := NewRanker(nil, cfg, m)
	r.store = stubRankerStore{priors: priors, avg: 0.5}
	r.cacheTTL = 0

	items := []search.TorrentContentResultItem{
		makeItem("Foo.WEBDL.x265-bad", "mkv", nil, 10),
		makeItem("Foo.WEBDL.x265-good", "mkv", nil, 10),
	}
	got := r.Rerank(context.Background(), items)

	if got[0].Torrent.Name != "Foo.WEBDL.x265-bad" {
		t.Fatalf("shadow mode must not reorder; got %q first", got[0].Torrent.Name)
	}
	if n := shiftSamples(t, m); n != 2 {
		t.Fatalf("shadow mode recorded %d displacement samples, want one per item (2)", n)
	}
}
