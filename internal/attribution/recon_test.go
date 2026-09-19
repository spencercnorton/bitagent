package attribution

import (
	"context"
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

func TestRow_Verdict(t *testing.T) {
	tests := []struct {
		name string
		row  Row
		want string
	}{
		{
			name: "imported_ok",
			row: Row{
				Grab:   GrabEvent{InfoHash: "abc"},
				QB:     QBState{Found: true, State: "uploading"},
				Import: ArrImport{Found: true, EventType: "downloadFolderImported"},
			},
			want: "imported_ok",
		},
		{
			name: "downloading",
			row: Row{
				Grab: GrabEvent{InfoHash: "abc"},
				QB:   QBState{Found: true, State: "downloading"},
			},
			want: "downloading",
		},
		{
			name: "stalled-via-stalledDL",
			row: Row{
				Grab: GrabEvent{InfoHash: "abc"},
				QB:   QBState{Found: true, State: "stalledDL"},
			},
			want: "stalled",
		},
		{
			name: "completed_pending_import",
			row: Row{
				Grab: GrabEvent{InfoHash: "abc"},
				QB:   QBState{Found: true, State: "stalledUP"},
			},
			want: "completed_pending_import",
		},
		{
			name: "missing_in_qb",
			row: Row{
				Grab: GrabEvent{InfoHash: "abc"},
			},
			want: "missing_in_qb",
		},
		{
			name: "import_failed",
			row: Row{
				Grab:   GrabEvent{InfoHash: "abc"},
				QB:     QBState{Found: true, State: "downloading"},
				Import: ArrImport{Found: true, EventType: "downloadFailed"},
			},
			want: "import_failed",
		},
		{
			name: "import_ignored_treated_as_failed",
			row: Row{
				Grab:   GrabEvent{InfoHash: "abc"},
				QB:     QBState{Found: true, State: "stalledUP"},
				Import: ArrImport{Found: true, EventType: "downloadIgnored"},
			},
			want: "import_failed",
		},
		{
			name: "stalled-via-error",
			row: Row{
				Grab: GrabEvent{InfoHash: "abc"},
				QB:   QBState{Found: true, State: "error"},
			},
			want: "stalled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.row.Verdict(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormaliseSource(t *testing.T) {
	tests := []struct {
		in   string
		want Source
	}{
		{"Sonarr", SourceSonarr},
		{"sonarr", SourceSonarr},
		{"  RADARR  ", SourceRadarr},
		{"Lidarr", SourceLidarr},
		{"", Source("")},
		{"Readarr", Source("")},
	}
	for _, tt := range tests {
		if got := normaliseSource(tt.in); got != tt.want {
			t.Errorf("normaliseSource(%q): got %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildGrabEvent_InfoHashFromMagnet(t *testing.T) {
	c := &prowlarrClient{indexerNames: map[int]string{1: "BitMagnet (Local DHT)"}}
	rec := prowlarrHistoryRecord{
		ID:        42,
		IndexerID: 1,
		Date:      "2026-04-25T10:00:00Z",
	}
	rec.Data.Source = "Sonarr"
	rec.Data.GrabTitle = "Some.Show.S01E01.1080p"
	rec.Data.URL = "magnet:?xt=urn:btih:DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF&dn=Some.Show"
	ev := c.buildGrabEvent(rec, time.Now())
	want := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if ev.InfoHash != want {
		t.Errorf("InfoHash: got %q, want %q", ev.InfoHash, want)
	}
	if ev.Source != SourceSonarr {
		t.Errorf("Source: got %v, want sonarr", ev.Source)
	}
	if ev.ProwlarrID != 42 {
		t.Errorf("ProwlarrID: got %d, want 42", ev.ProwlarrID)
	}
	if ev.Indexer != "BitMagnet (Local DHT)" {
		t.Errorf("Indexer: got %q, want %q", ev.Indexer, "BitMagnet (Local DHT)")
	}
	if ev.Title != "Some.Show.S01E01.1080p" {
		t.Errorf("Title (from data.grabTitle): got %q", ev.Title)
	}
}

func TestBuildGrabEvent_IndexerNameResolvedViaCache(t *testing.T) {
	// The Prowlarr history record carries indexerId only — the
	// human-readable name has to come from the cached lookup.
	c := &prowlarrClient{indexerNames: map[int]string{
		1: "BitMagnet (Local DHT)",
		2: "TorrentLeech",
	}}
	rec := prowlarrHistoryRecord{IndexerID: 2, Date: "2026-04-25T10:00:00Z"}
	rec.Data.URL = "magnet:?xt=urn:btih:1111111111111111111111111111111111111111"
	ev := c.buildGrabEvent(rec, time.Now())
	if ev.Indexer != "TorrentLeech" {
		t.Errorf("got %q, want TorrentLeech", ev.Indexer)
	}

	// Unknown indexer id → empty name (the substring filter then
	// skips the row when a filter is set).
	rec.IndexerID = 999
	ev = c.buildGrabEvent(rec, time.Now())
	if ev.Indexer != "" {
		t.Errorf("unknown indexer id: got %q, want empty", ev.Indexer)
	}

	// Nil cache (refresh failed) → empty name.
	c2 := &prowlarrClient{indexerNames: nil}
	ev = c2.buildGrabEvent(rec, time.Now())
	if ev.Indexer != "" {
		t.Errorf("nil cache: got %q, want empty", ev.Indexer)
	}
}

func TestBuildGrabEvent_LegacyFieldFallbacks(t *testing.T) {
	// Older Prowlarr versions used data.title / data.magnetUrl;
	// our parser keeps tolerating those for forward-compat.
	c := &prowlarrClient{indexerNames: map[int]string{1: "Old Indexer"}}
	rec := prowlarrHistoryRecord{IndexerID: 1, Date: "2026-04-25T10:00:00Z"}
	rec.Data.Title = "Legacy Title"
	rec.Data.MagnetURL = "magnet:?xt=urn:btih:CAFEBABECAFEBABECAFEBABECAFEBABECAFEBABE"
	ev := c.buildGrabEvent(rec, time.Now())
	if ev.Title != "Legacy Title" {
		t.Errorf("legacy title fallback: got %q", ev.Title)
	}
	if ev.InfoHash != "cafebabecafebabecafebabecafebabecafebabe" {
		t.Errorf("legacy magnetUrl fallback: got %q", ev.InfoHash)
	}
}

func TestBuildGrabEvent_PreferExplicitInfoHash(t *testing.T) {
	c := &prowlarrClient{indexerNames: map[int]string{1: "x"}}
	rec := prowlarrHistoryRecord{IndexerID: 1, Date: "2026-04-25T10:00:00Z"}
	rec.Data.InfoHash = "AAAA111122223333444455556666777788889999"
	rec.Data.URL = "magnet:?xt=urn:btih:DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF"
	ev := c.buildGrabEvent(rec, time.Now())
	// Explicit field takes precedence over magnet URL.
	if ev.InfoHash != "aaaa111122223333444455556666777788889999" {
		t.Errorf("explicit info_hash should win: got %q", ev.InfoHash)
	}
}

func TestNormaliseBTIH_HexAndBase32(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "lowercase hex passes through",
			in:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			want: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name: "uppercase hex lowercases",
			in:   "DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF",
			want: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name: "mixed case hex lowercases",
			in:   "DeAdBeEfDeAdBeEfDeAdBeEfDeAdBeEfDeAdBeEf",
			want: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name: "32-char base32 decodes to 40-char hex",
			// "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" hex
			// → 20 bytes → base32 = "32W353326W353326W353326W353326W3"
			// (Actual base32 of 20 zero bytes is "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			//  — using a known good fixture below.)
			in:   "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
			want: "", // Will compute expected below by round-trip
		},
		{
			name: "31-char garbage rejected",
			in:   "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PX",
			want: "",
		},
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "whitespace only",
			in:   "   ",
			want: "",
		},
		{
			name: "non-hex 40-char rejected",
			in:   "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ",
			want: "",
		},
	}
	// The base32 path is exercised by
	// TestNormaliseBTIH_Base32RoundTrip below using the real stdlib
	// encoder so we don't have to hard-code any fixture.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "32-char base32 decodes to 40-char hex" {
				// Skip — covered by the round-trip test below.
				t.Skip("covered by TestNormaliseBTIH_Base32RoundTrip")
			}
			got := normaliseBTIH(tt.in)
			if got != tt.want {
				t.Errorf("normaliseBTIH(%q): got %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormaliseBTIH_Base32RoundTrip(t *testing.T) {
	// Use the real stdlib encoder to build a base32 magnet and
	// verify normaliseBTIH round-trips it back to the original hex.
	wantHex := "48656c6c6f20576f726c642148656c6c6f212121" // 20 bytes, hex
	rawBytes := []byte{
		0x48, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x57, 0x6f, 0x72, 0x6c,
		0x64, 0x21, 0x48, 0x65, 0x6c, 0x6c, 0x6f, 0x21, 0x21, 0x21,
	}
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(rawBytes)
	if len(b32) != 32 {
		t.Fatalf("test fixture: expected 32-char base32, got %d (%q)", len(b32), b32)
	}
	got := normaliseBTIH(b32)
	if got != wantHex {
		t.Errorf("base32 round-trip: got %q, want %q", got, wantHex)
	}
	// Lowercase variant of the base32 should ALSO work (we
	// uppercase before decoding).
	got = normaliseBTIH(strings.ToLower(b32))
	if got != wantHex {
		t.Errorf("base32 lowercased: got %q, want %q", got, wantHex)
	}
}

func TestBuildGrabEvent_PriorityURLOverLegacy(t *testing.T) {
	// data.url (canonical) beats data.magnetUrl (legacy) when both
	// are present.
	c := &prowlarrClient{indexerNames: map[int]string{1: "x"}}
	rec := prowlarrHistoryRecord{IndexerID: 1, Date: "2026-04-25T10:00:00Z"}
	rec.Data.URL = "magnet:?xt=urn:btih:1111111111111111111111111111111111111111"
	rec.Data.MagnetURL = "magnet:?xt=urn:btih:2222222222222222222222222222222222222222"
	ev := c.buildGrabEvent(rec, time.Now())
	if ev.InfoHash != "1111111111111111111111111111111111111111" {
		t.Errorf("data.url should win: got %q", ev.InfoHash)
	}
}

func TestBuildGrabEvent_NoInfoHashFallsBackToEmpty(t *testing.T) {
	c := &prowlarrClient{indexerNames: map[int]string{1: "x"}}
	rec := prowlarrHistoryRecord{IndexerID: 1, Date: "2026-04-25T10:00:00Z"}
	ev := c.buildGrabEvent(rec, time.Now())
	if ev.InfoHash != "" {
		t.Errorf("no source for hash; expected empty, got %q", ev.InfoHash)
	}
}

func TestComputeSummary(t *testing.T) {
	rows := []Row{
		{
			Grab: GrabEvent{Indexer: "BitAgent (Local DHT)", Source: SourceSonarr},
			QB:   QBState{Found: true, State: "uploading"},
			Import: ArrImport{Found: true, EventType: "downloadFolderImported"},
		},
		{
			Grab: GrabEvent{Indexer: "BitMagnet (Local DHT)", Source: SourceRadarr},
			QB:   QBState{Found: true, State: "stalledDL"},
		},
		{
			Grab: GrabEvent{Indexer: "TorrentLeech (Prowlarr)", Source: SourceSonarr},
			QB:   QBState{Found: true, State: "downloading"},
		},
	}
	s := computeSummary(rows)
	if s.GrabsTotal != 3 {
		t.Errorf("GrabsTotal: got %d, want 3", s.GrabsTotal)
	}
	if s.BitAgentN != 2 {
		t.Errorf("BitAgentN: got %d, want 2 (one BitAgent + one BitMagnet legacy)", s.BitAgentN)
	}
	if s.BySource[SourceSonarr] != 2 {
		t.Errorf("Sonarr: got %d, want 2", s.BySource[SourceSonarr])
	}
	if s.BySource[SourceRadarr] != 1 {
		t.Errorf("Radarr: got %d, want 1", s.BySource[SourceRadarr])
	}
	if s.ByVerdict["imported_ok"] != 1 {
		t.Errorf("imported_ok: got %d, want 1", s.ByVerdict["imported_ok"])
	}
	if s.ByVerdict["stalled"] != 1 {
		t.Errorf("stalled: got %d, want 1", s.ByVerdict["stalled"])
	}
	if s.ByVerdict["downloading"] != 1 {
		t.Errorf("downloading: got %d, want 1", s.ByVerdict["downloading"])
	}
}

func TestFilterByIndexer(t *testing.T) {
	grabs := []GrabEvent{
		{Indexer: "BitMagnet (Local DHT)"},
		{Indexer: "TorrentLeech (Prowlarr)"},
		{Indexer: "1337x (Prowlarr)"},
		{Indexer: "BitMagnet (Local DHT)"},
	}
	got := filterByIndexer(grabs, "bitmagnet")
	if len(got) != 2 {
		t.Errorf("got %d, want 2 BitMagnet entries", len(got))
	}
	for _, g := range got {
		if g.Indexer != "BitMagnet (Local DHT)" {
			t.Errorf("unexpected indexer in filtered: %q", g.Indexer)
		}
	}
}

func TestRun_RequiresEnabled(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.ProwlarrBaseURL = "http://x"
	cfg.ProwlarrAPIKey = "k"
	_, err := Run(context.Background(), cfg, Filters{}, nil)
	if err == nil {
		t.Errorf("expected error when ATTRIBUTION_ENABLED=false")
	}
}

func TestRun_RequiresProwlarr(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	_, err := Run(context.Background(), cfg, Filters{}, nil)
	if err == nil {
		t.Errorf("expected error when Prowlarr URL+key missing")
	}
}

func TestConfig_HasProwlarrAndQB(t *testing.T) {
	c := NewDefaultConfig()
	if c.HasProwlarr() {
		t.Errorf("default HasProwlarr should be false")
	}
	if c.HasQB() {
		t.Errorf("default HasQB should be false")
	}

	c.ProwlarrBaseURL = "http://x"
	c.ProwlarrAPIKey = "k"
	if !c.HasProwlarr() {
		t.Errorf("set both fields, should be true")
	}

	c.QBTBaseURL = "http://y"
	if c.HasQB() {
		t.Errorf("user/pass missing, should be false")
	}
	c.QBTUsername = "u"
	if !c.HasQB() {
		t.Errorf("set user, should be true")
	}
}
