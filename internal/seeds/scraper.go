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
// that is TrackerKnown but not Positive has reported zero active peers
// (known-zero). This observation alone does not establish swarm death.
// A hash that is not TrackerKnown has unresolved tracker coverage.
func (o ScrapeOutcome) Positive() bool { return o.Seeders > 0 || o.Leechers > 0 }

// Class buckets the outcome for metrics/reporting: "positive" (live swarm),
// "known_zero" (a tracker knows the hash but reports zero active peers), or
// "unknown" (no tracker in the pool established knowledge of it).
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
	admit   func(context.Context, [][]byte, func([][]byte) error) error
	scrape  func(context.Context, string, []infohash.T) ([]scrapeItem, error)
}

// NewScraper builds a Scraper. metrics may be nil (the CLI path constructs its
// own); per-tracker counters are guarded accordingly.
func NewScraper(cfg Config, metrics *Metrics, logger *zap.SugaredLogger) *Scraper {
	return &Scraper{cfg: cfg, metrics: metrics, logger: logger}
}

// ScrapeBatch scrapes hashes across the tracker pool concurrently and returns
// the best outcome per hash, keyed by the raw 20-byte info_hash string. Every
// admitted input hash gets an entry (unknown outcomes included). Denied hashes
// are omitted, and failed admission is an error instead of a new unknown fact.
func (s *Scraper) ScrapeBatch(ctx context.Context, hashes [][]byte) (map[string]*ScrapeOutcome, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	var admissionErr error
	withheld := make(map[string]bool)
	reportAdmissionError := func(err error) {
		mu.Lock()
		if admissionErr == nil {
			admissionErr = err
		}
		mu.Unlock()
		cancel()
	}

	for _, url := range s.cfg.TrackerUrls {
		select {
		case <-ctx.Done():
			wg.Wait()
			if admissionErr == nil {
				admissionErr = ctx.Err()
			}
			return out, admissionErr
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(trackerURL string) {
			defer wg.Done()
			defer func() { <-sem }()
			s.scrapeOneTracker(ctx, trackerURL, ihs, hashes, out, &mu, packetAdmission{fail: reportAdmissionError, withheld: withheld})
		}(url)
	}
	wg.Wait()
	if admissionErr == nil {
		admissionErr = ctx.Err()
	}
	return out, admissionErr
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
	admission packetAdmission,
) {
	var connected chunkScrapeFunc
	var closeClient func()
	defer func() {
		if closeClient != nil {
			closeClient()
		}
	}()
	// scrape wraps the anacrolix client with our per-packet timeout and adapts
	// its result into the network-free scrapeItem slice, so the chunk loop's
	// retry / continue / abandon policy lives in the testable
	// scrapeTrackerChunks.
	scrape := func(ctx context.Context, chunk []infohash.T) ([]scrapeItem, error) {
		if s.scrape != nil {
			return s.scrape(ctx, trackerURL, chunk)
		}
		cctx, cancel := context.WithTimeout(ctx, s.cfg.ScrapeTimeout)
		defer cancel()
		if connected == nil {
			cl, err := tracker.NewClient(trackerURL, tracker.NewClientOpts{})
			if err != nil {
				return nil, err
			}
			closeClient = func() { _ = cl.Close() }
			connected = func(ctx context.Context, chunk []infohash.T) ([]scrapeItem, error) {
				resp, err := cl.Scrape(ctx, chunk)
				if err != nil {
					return nil, err
				}
				items := make([]scrapeItem, len(resp))
				for i := range resp {
					items[i] = scrapeItem{Seeders: resp[i].Seeders, Leechers: resp[i].Leechers, Completed: resp[i].Completed}
				}
				return items, nil
			}
		}
		return connected(cctx, chunk)
	}
	s.scrapeTrackerChunks(ctx, trackerURL, ihs, hashes, out, mu, scrape, admission)
}

// A denial is monotonic within one batch. A delayed earlier tracker response
// cannot recreate a withheld hash; a later ordinary batch may recheck it.
type packetAdmission struct {
	fail     func(error)
	withheld map[string]bool
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
	admissions ...packetAdmission,
) {
	var admission packetAdmission
	if len(admissions) > 0 {
		admission = admissions[0]
	}
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

		var items []scrapeItem
		var err error
		var admitted [][]byte
		for attempt := 0; attempt <= chunkScrapeRetries; attempt++ {
			if ctx.Err() != nil {
				return
			}
			call := func(allowed [][]byte) error {
				allowedSet := map[string]bool{}
				for _, h := range allowed {
					allowedSet[string(h)] = true
				}
				mu.Lock()
				for _, h := range hashes[start:end] {
					if !allowedSet[string(h)] {
						if admission.withheld != nil {
							admission.withheld[string(h)] = true
						}
						delete(out, string(h))
					}
				}
				admitted = nil
				filtered := make([]infohash.T, 0, len(allowed))
				for _, h := range allowed {
					if admission.withheld[string(h)] {
						continue
					}
					admitted = append(admitted, h)
					var id infohash.T
					copy(id[:], h)
					filtered = append(filtered, id)
				}
				mu.Unlock()
				if len(filtered) == 0 {
					return nil
				}
				items, err = scrape(ctx, filtered)
				return err
			}
			if s.admit == nil {
				err = call(hashes[start:end])
			} else {
				// Source locks serialize overlapping hashes across tracker fanout.
				// Allow their bounded queue to drain; each actual network call still
				// has the unchanged ScrapeTimeout, concurrency and retry limits.
				queueBudget := s.cfg.ScrapeTimeout*time.Duration(max(1, s.cfg.Concurrency)+1) + 2*time.Second
				packetCtx, packetCancel := context.WithTimeout(ctx, queueBudget)
				var networkErr error
				gateErr := s.admit(packetCtx, hashes[start:end], func(allowed [][]byte) error { networkErr = call(allowed); return networkErr })
				packetCancel()
				if gateErr != nil && networkErr == nil {
					if admission.fail != nil {
						admission.fail(gateErr)
					}
					return
				}
				err = networkErr
			}
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

		if len(admitted) == 0 {
			continue
		}
		consecutiveFails = 0
		scrapedAny = true
		s.trackerResult(trackerURL, "ok")

		mu.Lock()
		for i := 0; i < len(items) && i < len(admitted); i++ {
			it := items[i]
			key := string(admitted[i])
			if admission.withheld[key] {
				continue
			}
			if out[key] == nil {
				out[key] = &ScrapeOutcome{}
			}
			mergeResult(out[key], trackerURL, it.Seeders, it.Leechers, it.Completed)
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
