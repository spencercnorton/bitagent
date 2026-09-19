package seeds

import (
	"context"
	"sync"
	"time"

	"github.com/anacrolix/torrent/tracker"
	"github.com/anacrolix/torrent/types/infohash"
	"go.uber.org/zap"
)

// ScrapeOutcome is the aggregated result for one info-hash across the whole
// tracker pool. Seeders/Leechers/Completed are the max seen at any single
// tracker (a hash present on any tracker is found); BestTracker is the tracker
// that reported the max seeders.
type ScrapeOutcome struct {
	TrackerKnown bool
	Seeders      int32
	Leechers     int32
	Completed    int32
	BestTracker  string
}

// Positive reports a live swarm — the authoritative signal we surface. A hash
// that is TrackerKnown but not Positive is a real dead swarm (known-zero); a
// hash that is not TrackerKnown is an honest unknown.
func (o ScrapeOutcome) Positive() bool { return o.Seeders > 0 || o.Leechers > 0 }

// Class buckets the outcome for metrics/reporting: "positive" (live swarm),
// "known_zero" (a tracker has the hash but it's dead), or "unknown" (no tracker
// in the pool knows it).
func (o ScrapeOutcome) Class() string {
	switch {
	case o.Positive():
		return "positive"
	case o.TrackerKnown:
		return "known_zero"
	default:
		return "unknown"
	}
}

// mergeResult folds one tracker's scrape numbers for a single hash into the
// running cross-pool outcome. Seeders/Leechers/Completed are taken as the max
// across trackers; BestTracker follows the max seeders. A tracker reporting all
// zeros does not know the hash and is ignored.
func mergeResult(o *ScrapeOutcome, trackerURL string, seeders, leechers, completed int32) {
	if seeders <= 0 && leechers <= 0 && completed <= 0 {
		return
	}
	o.TrackerKnown = true
	if seeders > o.Seeders {
		o.Seeders = seeders
		o.BestTracker = trackerURL
	}
	if leechers > o.Leechers {
		o.Leechers = leechers
	}
	if completed > o.Completed {
		o.Completed = completed
	}
}

// Scraper performs BEP-15 UDP tracker scrapes across the configured pool. The
// anacrolix/torrent tracker client owns the wire protocol (connect handshake,
// scrape packet); this type adds chunking, pool fan-out, per-hash max
// aggregation and pacing.
type Scraper struct {
	cfg     Config
	metrics *Metrics
	logger  *zap.SugaredLogger
}

// NewScraper builds a Scraper. metrics may be nil (the CLI path constructs its
// own); per-tracker counters are guarded accordingly.
func NewScraper(cfg Config, metrics *Metrics, logger *zap.SugaredLogger) *Scraper {
	return &Scraper{cfg: cfg, metrics: metrics, logger: logger}
}

// ScrapeBatch scrapes hashes across the tracker pool concurrently and returns
// the best outcome per hash, keyed by the raw 20-byte info_hash string. Every
// input hash gets an entry (unknown outcomes included) so the caller can record
// a checked_at for all of them.
func (s *Scraper) ScrapeBatch(ctx context.Context, hashes [][]byte) map[string]*ScrapeOutcome {
	ihs := make([]infohash.T, len(hashes))
	for i, h := range hashes {
		var t infohash.T
		copy(t[:], h)
		ihs[i] = t
	}

	out := make(map[string]*ScrapeOutcome, len(hashes))
	for _, h := range hashes {
		out[string(h)] = &ScrapeOutcome{}
	}

	var mu sync.Mutex
	sem := make(chan struct{}, max(1, s.cfg.Concurrency))
	var wg sync.WaitGroup

	for _, url := range s.cfg.TrackerUrls {
		select {
		case <-ctx.Done():
			wg.Wait()
			return out
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(trackerURL string) {
			defer wg.Done()
			defer func() { <-sem }()
			s.scrapeOneTracker(ctx, trackerURL, ihs, hashes, out, &mu)
		}(url)
	}
	wg.Wait()
	return out
}

// scrapeOneTracker connects to a single tracker and scrapes every chunk of the
// batch, merging results into out under mu. A tracker that fails its first
// packet is abandoned for this cycle (it is almost certainly down or does not
// support scrape); the error is counted once.
func (s *Scraper) scrapeOneTracker(
	ctx context.Context,
	trackerURL string,
	ihs []infohash.T,
	hashes [][]byte,
	out map[string]*ScrapeOutcome,
	mu *sync.Mutex,
) {
	cl, err := tracker.NewClient(trackerURL, tracker.NewClientOpts{})
	if err != nil {
		s.trackerResult(trackerURL, "error")
		s.logger.Debugw("seeds: tracker client init failed", "tracker", trackerURL, "err", err)
		return
	}
	defer cl.Close()

	// scrape wraps the anacrolix client with our per-packet timeout and adapts
	// its result into the network-free scrapeItem slice, so the chunk loop's
	// retry / continue / abandon policy lives in the testable
	// scrapeTrackerChunks.
	scrape := func(ctx context.Context, chunk []infohash.T) ([]scrapeItem, error) {
		cctx, cancel := context.WithTimeout(ctx, s.cfg.ScrapeTimeout)
		defer cancel()
		resp, err := cl.Scrape(cctx, chunk)
		if err != nil {
			return nil, err
		}
		items := make([]scrapeItem, len(resp))
		for i := range resp {
			items[i] = scrapeItem{Seeders: resp[i].Seeders, Leechers: resp[i].Leechers, Completed: resp[i].Completed}
		}
		return items, nil
	}
	s.scrapeTrackerChunks(ctx, trackerURL, ihs, hashes, out, mu, scrape)
}

const (
	// chunkScrapeRetries retransmits a scrape packet that fails before giving
	// up on that chunk. UDP is lossy and public trackers drop packets under
	// sustained load, so a single failure is almost always transient — not a
	// dead tracker.
	chunkScrapeRetries = 1
	// maxConsecutiveChunkFails abandons a tracker that fails this many chunks
	// in a row *after* it has already answered at least one — i.e. it went
	// unresponsive mid-stream (rate-limited) rather than being down.
	maxConsecutiveChunkFails = 4
)

// scrapeItem is the per-infohash numbers taken from a tracker scrape response,
// decoupled from the anacrolix result type so the chunk loop is unit-testable
// without a live network client.
type scrapeItem struct {
	Seeders, Leechers, Completed int32
}

// chunkScrapeFunc scrapes one chunk of infohashes against a single tracker,
// returning per-hash numbers index-aligned with the chunk (a tracker may
// return a shorter prefix).
type chunkScrapeFunc func(ctx context.Context, chunk []infohash.T) ([]scrapeItem, error)

// scrapeTrackerChunks scrapes one tracker over the whole batch in packet-sized
// chunks, merging results into out under mu. It is the network-free core of
// scrapeOneTracker.
//
// Policy (the reason this exists): a scrape that fails is retransmitted
// (chunkScrapeRetries) because UDP loss is normal. A chunk that still fails is
// SKIPPED, not fatal — the old code abandoned the entire tracker on the first
// dropped packet, so with a large batch a single loss ~40 chunks in forfeited
// the remaining thousands of hashes (which were then wrongly stamped
// checked/unknown and locked out for MinRescrapeAge). We only abandon the
// tracker when it never answers a single chunk (down / no scrape support) or
// goes silent for maxConsecutiveChunkFails chunks in a row after answering.
func (s *Scraper) scrapeTrackerChunks(
	ctx context.Context,
	trackerURL string,
	ihs []infohash.T,
	hashes [][]byte,
	out map[string]*ScrapeOutcome,
	mu *sync.Mutex,
	scrape chunkScrapeFunc,
) {
	limit := s.cfg.MaxHashesPerPacket
	if limit <= 0 {
		limit = 70
	}

	scrapedAny := false
	consecutiveFails := 0
	for start := 0; start < len(ihs); start += limit {
		if ctx.Err() != nil {
			return
		}
		end := min(start+limit, len(ihs))
		chunk := ihs[start:end]

		var items []scrapeItem
		var err error
		for attempt := 0; attempt <= chunkScrapeRetries; attempt++ {
			if ctx.Err() != nil {
				return
			}
			items, err = scrape(ctx, chunk)
			if err == nil {
				break
			}
		}

		if err != nil {
			s.trackerResult(trackerURL, "error")
			s.logger.Debugw("seeds: tracker scrape chunk failed", "tracker", trackerURL, "err", err)
			if !scrapedAny {
				// Never answered a single chunk — down or no scrape support.
				return
			}
			consecutiveFails++
			if consecutiveFails >= maxConsecutiveChunkFails {
				s.logger.Debugw("seeds: tracker abandoned mid-stream",
					"tracker", trackerURL, "consecutive_fails", consecutiveFails)
				return
			}
			// Transient loss on this chunk — skip just these hashes and keep
			// going so a dropped packet doesn't forfeit the rest of the batch.
			continue
		}

		consecutiveFails = 0
		scrapedAny = true
		s.trackerResult(trackerURL, "ok")

		mu.Lock()
		for i := 0; i < len(items) && i < len(chunk); i++ {
			it := items[i]
			mergeResult(out[string(hashes[start+i])], trackerURL, it.Seeders, it.Leechers, it.Completed)
		}
		mu.Unlock()

		// Pace successive packets to the same tracker.
		if end < len(ihs) && s.cfg.PerTrackerInterval > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.cfg.PerTrackerInterval):
			}
		}
	}
}

func (s *Scraper) trackerResult(trackerURL, result string) {
	if s.metrics != nil {
		s.metrics.trackerScrapes.WithLabelValues(trackerURL, result).Inc()
	}
}
