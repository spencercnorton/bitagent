package seeds

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/types/infohash"
	"go.uber.org/zap"
)

func TestMergeResult_MaxAcrossTrackers(t *testing.T) {
	o := &ScrapeOutcome{}

	// Tracker A: modest live swarm.
	mergeResult(o, "udp://a", 5, 2, 10)
	// Tracker B: bigger seeder count — should win seeders + BestTracker.
	mergeResult(o, "udp://b", 12, 1, 40)
	// Tracker C: more leechers than either, fewer seeders.
	mergeResult(o, "udp://c", 3, 9, 5)

	if !o.TrackerKnown {
		t.Fatal("expected TrackerKnown")
	}
	if o.Seeders != 12 {
		t.Errorf("seeders = %d, want 12 (max)", o.Seeders)
	}
	if o.Leechers != 9 {
		t.Errorf("leechers = %d, want 9 (max)", o.Leechers)
	}
	if o.Completed != 40 {
		t.Errorf("completed = %d, want 40 (max)", o.Completed)
	}
	if o.BestTracker != "udp://b" {
		t.Errorf("best tracker = %q, want udp://b (max seeders)", o.BestTracker)
	}
	if o.Class() != "positive" {
		t.Errorf("class = %q, want positive", o.Class())
	}
}

func TestMergeResult_AllZeroIgnored(t *testing.T) {
	o := &ScrapeOutcome{}
	// A tracker that returns all zeros does not know the hash.
	mergeResult(o, "udp://a", 0, 0, 0)
	if o.TrackerKnown {
		t.Error("all-zero result must not mark TrackerKnown")
	}
	if o.Class() != "unknown" {
		t.Errorf("class = %q, want unknown", o.Class())
	}
}

func TestMergeResult_KnownZero(t *testing.T) {
	o := &ScrapeOutcome{}
	// Completed>0 with no current peers = a tracker knows the hash but the
	// swarm is dead. This is a real zero, distinct from unknown.
	mergeResult(o, "udp://a", 0, 0, 17)
	if !o.TrackerKnown {
		t.Fatal("completed>0 must mark TrackerKnown")
	}
	if o.Positive() {
		t.Error("no seeders/leechers must not be Positive")
	}
	if o.Class() != "known_zero" {
		t.Errorf("class = %q, want known_zero", o.Class())
	}
}

func TestScrapeOutcome_Positive(t *testing.T) {
	cases := []struct {
		name string
		o    ScrapeOutcome
		want bool
	}{
		{"seeders only", ScrapeOutcome{Seeders: 3}, true},
		{"leechers only", ScrapeOutcome{Leechers: 1}, true},
		{"completed only", ScrapeOutcome{TrackerKnown: true, Completed: 9}, false},
		{"empty", ScrapeOutcome{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.o.Positive(); got != c.want {
				t.Errorf("Positive() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDefaultConfig_TrackerPoolNonEmpty(t *testing.T) {
	cfg := NewDefaultConfig()
	if len(cfg.TrackerUrls) == 0 {
		t.Fatal("default tracker pool must be non-empty")
	}
	if cfg.Enabled || cfg.EnableWrite {
		t.Error("defaults must be opt-in (Enabled=false, EnableWrite=false)")
	}
	if cfg.MaxHashesPerPacket > 74 {
		t.Errorf("MaxHashesPerPacket = %d, must stay <= 74 (BEP-15 packet limit)", cfg.MaxHashesPerPacket)
	}
}

// --- scrapeTrackerChunks retry / continue / abandon policy ---

func newTestScraper(cfg Config) *Scraper {
	return NewScraper(cfg, nil, zap.NewNop().Sugar())
}

// testHashes builds n distinct 20-byte hashes, their infohash.T forms, and an
// initialized outcome map. The 0-based batch index is stamped in the first
// byte (1-based) so a test's scrape func can identify the chunk.
func testHashes(n int) (hashes [][]byte, ihs []infohash.T, out map[string]*ScrapeOutcome) {
	hashes = make([][]byte, n)
	ihs = make([]infohash.T, n)
	out = make(map[string]*ScrapeOutcome, n)
	for i := 0; i < n; i++ {
		b := make([]byte, 20)
		b[0] = byte(i + 1)
		hashes[i] = b
		copy(ihs[i][:], b)
		out[string(b)] = &ScrapeOutcome{}
	}
	return hashes, ihs, out
}

// chunkIndex recovers the 0-based batch position of a single-hash chunk. Tests
// use MaxHashesPerPacket=1 so every chunk holds exactly one hash.
func chunkIndex(chunk []infohash.T) int { return int(chunk[0][0]) - 1 }

func TestScrapeTrackerChunks_RetryRecoversTransientLoss(t *testing.T) {
	s := newTestScraper(Config{MaxHashesPerPacket: 1})
	hashes, ihs, out := testHashes(1)
	var mu sync.Mutex
	calls := 0
	scrape := func(_ context.Context, _ []infohash.T) ([]scrapeItem, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("udp timeout") // first packet dropped
		}
		return []scrapeItem{{Seeders: 7, Leechers: 2, Completed: 3}}, nil
	}
	s.scrapeTrackerChunks(context.Background(), "udp://t", ihs, hashes, out, &mu, scrape)

	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (1 drop + 1 retransmit)", calls)
	}
	o := out[string(hashes[0])]
	if !o.TrackerKnown || o.Seeders != 7 || o.Leechers != 2 {
		t.Errorf("retransmit should have merged the chunk, got %+v", o)
	}
}

func TestScrapeTrackerChunks_SkipsBadChunkKeepsTracker(t *testing.T) {
	s := newTestScraper(Config{MaxHashesPerPacket: 1})
	hashes, ihs, out := testHashes(3)
	var mu sync.Mutex
	scrape := func(_ context.Context, chunk []infohash.T) ([]scrapeItem, error) {
		if chunkIndex(chunk) == 1 {
			return nil, errors.New("drop") // chunk 1 fails every attempt
		}
		return []scrapeItem{{Seeders: int32(10 + chunkIndex(chunk))}}, nil
	}
	s.scrapeTrackerChunks(context.Background(), "udp://t", ihs, hashes, out, &mu, scrape)

	if out[string(hashes[0])].Seeders != 10 {
		t.Errorf("hash0 seeders = %d, want 10", out[string(hashes[0])].Seeders)
	}
	if out[string(hashes[1])].TrackerKnown {
		t.Error("hash1 (persistently-failing chunk) must be left unknown, not fatal")
	}
	// Key assertion: a dropped chunk does NOT abandon the tracker — chunk 2 is
	// still scraped (the old code would have returned at chunk 1).
	if out[string(hashes[2])].Seeders != 12 {
		t.Errorf("hash2 seeders = %d, want 12 (tracker must continue past a bad chunk)", out[string(hashes[2])].Seeders)
	}
}

func TestScrapeTrackerChunks_AbandonsDownTracker(t *testing.T) {
	s := newTestScraper(Config{MaxHashesPerPacket: 1})
	hashes, ihs, out := testHashes(5)
	var mu sync.Mutex
	calls := 0
	scrape := func(_ context.Context, _ []infohash.T) ([]scrapeItem, error) {
		calls++
		return nil, errors.New("down")
	}
	s.scrapeTrackerChunks(context.Background(), "udp://t", ihs, hashes, out, &mu, scrape)

	// Never answered a single chunk → abandon after the first chunk's attempts;
	// later chunks are not attempted.
	if calls != chunkScrapeRetries+1 {
		t.Errorf("calls = %d, want %d (only chunk 0 attempted, then abandon)", calls, chunkScrapeRetries+1)
	}
	for i := range hashes {
		if out[string(hashes[i])].TrackerKnown {
			t.Errorf("hash%d must be unknown after abandoning a down tracker", i)
		}
	}
}

func TestScrapeTrackerChunks_AbandonsMidStreamAfterConsecutiveFails(t *testing.T) {
	s := newTestScraper(Config{MaxHashesPerPacket: 1})
	n := maxConsecutiveChunkFails + 3
	hashes, ihs, out := testHashes(n)
	var mu sync.Mutex
	lastAttempted := -1
	scrape := func(_ context.Context, chunk []infohash.T) ([]scrapeItem, error) {
		idx := chunkIndex(chunk)
		if idx > lastAttempted {
			lastAttempted = idx
		}
		if idx == 0 {
			return []scrapeItem{{Seeders: 5}}, nil // answers once
		}
		return nil, errors.New("gone dark") // then fails every chunk
	}
	s.scrapeTrackerChunks(context.Background(), "udp://t", ihs, hashes, out, &mu, scrape)

	// chunk 0 answers, then chunks 1..maxConsecutiveChunkFails fail in a row and
	// the tracker is abandoned — nothing beyond index maxConsecutiveChunkFails
	// is attempted.
	if lastAttempted != maxConsecutiveChunkFails {
		t.Errorf("lastAttempted = %d, want %d", lastAttempted, maxConsecutiveChunkFails)
	}
	if out[string(hashes[0])].Seeders != 5 {
		t.Errorf("hash0 seeders = %d, want 5", out[string(hashes[0])].Seeders)
	}
	if out[string(hashes[n-1])].TrackerKnown {
		t.Error("final hash must be unattempted after mid-stream abandon")
	}
}
