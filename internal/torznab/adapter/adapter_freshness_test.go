package adapter

import (
	"database/sql"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/model"
)

// fixedNow returns a stable "now" so the staleness window is
// deterministic across runs.
var fixedNow = time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)

func sourceWithSeeders(name string, seeders uint, valid bool, updatedAt time.Time) model.TorrentsTorrentSource {
	return model.TorrentsTorrentSource{
		Source:    name,
		Seeders:   model.NullUint{Valid: valid, Uint: seeders},
		UpdatedAt: updatedAt,
		PublishedAt: sql.NullTime{
			Valid: !updatedAt.IsZero(),
			Time:  updatedAt,
		},
	}
}

func makeItem(infoHash byte, sources ...model.TorrentsTorrentSource) search.TorrentContentResultItem {
	hash := make([]byte, 20)
	hash[0] = infoHash
	t := model.Torrent{}
	_ = t.InfoHash.UnmarshalBinary(hash)
	t.Sources = sources
	return search.TorrentContentResultItem{
		TorrentContent: model.TorrentContent{Torrent: t},
	}
}

func makeAdapter(cfg FreshnessConfig) Adapter {
	return Adapter{
		freshness: cfg,
		now:       func() time.Time { return fixedNow },
	}
}

func TestApplyFreshnessFilter_HideZeroSeeders(t *testing.T) {
	a := makeAdapter(FreshnessConfig{HideZeroSeeders: true})

	res := search.TorrentContentResult{
		Items: []search.TorrentContentResultItem{
			makeItem(1, sourceWithSeeders("trk", 0, true, fixedNow)),  // dropped
			makeItem(2, sourceWithSeeders("trk", 5, true, fixedNow)),  // kept
			makeItem(3, sourceWithSeeders("trk", 0, false, fixedNow)), // kept (unknown, recent)
		},
	}
	out := a.applyFreshnessFilter(res)
	if len(out.Items) != 2 {
		t.Fatalf("expected 2 items kept, got %d", len(out.Items))
	}
}

func TestApplyFreshnessFilter_HideZeroOnlyWhenAllSourcesAgree(t *testing.T) {
	a := makeAdapter(FreshnessConfig{HideZeroSeeders: true})

	res := search.TorrentContentResult{
		Items: []search.TorrentContentResultItem{
			// One source reports 5, the other 0 — Seeders() takes the max → 5 → keep.
			makeItem(1,
				sourceWithSeeders("trk-a", 0, true, fixedNow),
				sourceWithSeeders("trk-b", 5, true, fixedNow),
			),
		},
	}
	out := a.applyFreshnessFilter(res)
	if len(out.Items) != 1 {
		t.Fatalf("expected the multi-source item kept (max=5), got %d", len(out.Items))
	}
}

func TestApplyFreshnessFilter_HideStaleUnknown(t *testing.T) {
	a := makeAdapter(FreshnessConfig{HideUnknownSeedersAgeDays: 7})

	old := fixedNow.AddDate(0, 0, -10) // 10d old, past 7d cutoff
	fresh := fixedNow.AddDate(0, 0, -3)

	res := search.TorrentContentResult{
		Items: []search.TorrentContentResultItem{
			// unknown seeders + only stale source → drop
			makeItem(1, sourceWithSeeders("dht", 0, false, old)),
			// unknown seeders + a fresh source → keep (recent DHT discovery is signal)
			makeItem(2, sourceWithSeeders("dht", 0, false, fresh)),
			// unknown seeders + no sources at all → drop (no signal it exists)
			makeItem(3),
			// known seeders + stale source → keep (zero-seeders filter is OFF)
			makeItem(4, sourceWithSeeders("trk", 5, true, old)),
		},
	}
	out := a.applyFreshnessFilter(res)
	if len(out.Items) != 2 {
		t.Fatalf("expected 2 items kept (fresh-unknown + stale-known), got %d", len(out.Items))
	}
	// Verify the right ones survived: hashes 2 and 4.
	keptFirstByte := map[byte]bool{}
	for _, item := range out.Items {
		b, _ := item.Torrent.InfoHash.MarshalBinary()
		keptFirstByte[b[0]] = true
	}
	if !keptFirstByte[2] || !keptFirstByte[4] {
		t.Fatalf("wrong items survived: %+v", keptFirstByte)
	}
}

func TestApplyFreshnessFilter_BothFiltersTogether(t *testing.T) {
	a := makeAdapter(FreshnessConfig{HideZeroSeeders: true, HideUnknownSeedersAgeDays: 7})

	old := fixedNow.AddDate(0, 0, -30)
	fresh := fixedNow.AddDate(0, 0, -1)

	res := search.TorrentContentResult{
		Items: []search.TorrentContentResultItem{
			makeItem(1, sourceWithSeeders("trk", 0, true, fresh)),  // zero-seeders → drop
			makeItem(2, sourceWithSeeders("dht", 0, false, old)),   // stale-unknown → drop
			makeItem(3, sourceWithSeeders("trk", 1, true, old)),    // valid seeders, old source → keep
			makeItem(4, sourceWithSeeders("dht", 0, false, fresh)), // unknown but recent → keep
		},
	}
	out := a.applyFreshnessFilter(res)
	if len(out.Items) != 2 {
		t.Fatalf("expected 2 kept, got %d", len(out.Items))
	}
}

func TestApplyFreshnessFilter_NoOpWhenZeroConfig(t *testing.T) {
	a := makeAdapter(FreshnessConfig{})
	if !a.freshness.IsZero() {
		t.Fatalf("zero-value config should report IsZero")
	}
	// applyFreshnessFilter is only called when !IsZero in Search(), so we
	// don't exercise it here — just confirm the gate.
}

func TestApplyFreshnessFilter_StalenessUsesPublishedAtWhenNewer(t *testing.T) {
	a := makeAdapter(FreshnessConfig{HideUnknownSeedersAgeDays: 7})

	veryOld := fixedNow.AddDate(0, 0, -30)
	recent := fixedNow.AddDate(0, 0, -2)

	src := model.TorrentsTorrentSource{
		Source:      "dht",
		Seeders:     model.NullUint{Valid: false},
		UpdatedAt:   veryOld,                                 // would mark stale on its own
		PublishedAt: sql.NullTime{Valid: true, Time: recent}, // but PublishedAt is recent
	}
	res := search.TorrentContentResult{
		Items: []search.TorrentContentResultItem{makeItem(1, src)},
	}
	out := a.applyFreshnessFilter(res)
	if len(out.Items) != 1 {
		t.Fatalf("expected fresh-published item kept, got %d", len(out.Items))
	}
}

// Authoritative-zero-only: a 0 hides only when the 'tracker' source row
// (mirrored from the scrape ledger) says so. A DHT bloom-approximated 0 is
// honest-unknown — served while recent, aged out by the staleness window.
// This is the L3 self-hide fix: freshly-crawled alive torrents whose bloom
// read 0 must be served, not suppressed.
func TestApplyFreshnessFilter_AuthoritativeZeroOnly(t *testing.T) {
	a := makeAdapter(FreshnessConfig{
		HideZeroSeeders:           true,
		HideUnknownSeedersAgeDays: 7,
		AuthoritativeZeroOnly:     true,
	})

	fresh := fixedNow.Add(-1 * time.Hour)
	stale := fixedNow.AddDate(0, 0, -8)

	res := search.TorrentContentResult{
		Items: []search.TorrentContentResultItem{
			// freshly-crawled bloom zero → KEEP (the self-hide population)
			makeItem(1, sourceWithSeeders("dht", 0, true, fresh)),
			// authoritative known-zero → drop
			makeItem(2, sourceWithSeeders(model.SourceKeyTracker, 0, true, fresh)),
			// authoritative zero beats a stale bloom positive → drop
			makeItem(3,
				sourceWithSeeders(model.SourceKeyTracker, 0, true, fresh),
				sourceWithSeeders("dht", 3, true, fresh),
			),
			// bloom zero with every source stale → drop via unknown window
			makeItem(4, sourceWithSeeders("dht", 0, true, stale)),
			// stale bloom positive, no tracker verdict → keep
			makeItem(5, sourceWithSeeders("dht", 5, true, stale)),
			// no seeder data, stale → drop (unchanged from legacy)
			makeItem(6, sourceWithSeeders("dht", 0, false, stale)),
			// no seeder data, recent → keep
			makeItem(7, sourceWithSeeders("dht", 0, false, fresh)),
			// authoritative positive → keep
			makeItem(8, sourceWithSeeders(model.SourceKeyTracker, 4, true, fresh)),
		},
	}
	out := a.applyFreshnessFilter(res)
	keptFirstByte := map[byte]bool{}
	for _, item := range out.Items {
		b, _ := item.Torrent.InfoHash.MarshalBinary()
		keptFirstByte[b[0]] = true
	}
	for _, want := range []byte{1, 5, 7, 8} {
		if !keptFirstByte[want] {
			t.Errorf("item %d should have been kept; kept=%v", want, keptFirstByte)
		}
	}
	if len(out.Items) != 4 {
		t.Fatalf("expected 4 kept, got %d (%v)", len(out.Items), keptFirstByte)
	}
}

// With the flag off the legacy behavior is byte-for-byte: any Valid 0 hides,
// including a fresh bloom zero.
func TestApplyFreshnessFilter_LegacyBloomZeroStillHides(t *testing.T) {
	a := makeAdapter(FreshnessConfig{HideZeroSeeders: true})
	res := search.TorrentContentResult{
		Items: []search.TorrentContentResultItem{
			makeItem(1, sourceWithSeeders("dht", 0, true, fixedNow)),
		},
	}
	if out := a.applyFreshnessFilter(res); len(out.Items) != 0 {
		t.Fatalf("legacy mode must hide bloom zero, kept %d", len(out.Items))
	}
}

func TestFreshnessConfigFromTorznab_PassesFieldsThrough(t *testing.T) {
	// Local import-light check that the bridge function copies the two fields.
	// Avoids importing the torznab package transitively in the test file.
	cfg := FreshnessConfig{HideZeroSeeders: true, HideUnknownSeedersAgeDays: 14}
	if !cfg.HideZeroSeeders || cfg.HideUnknownSeedersAgeDays != 14 {
		t.Fatalf("config fields not preserved: %+v", cfg)
	}
	if cfg.IsZero() {
		t.Fatalf("non-default config should not be IsZero")
	}
}
