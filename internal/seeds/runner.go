package seeds

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/evidence/liveness"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"go.uber.org/zap"
)

// LivenessRecorder is the slice of *liveness.Store the runner feeds scrape
// evidence into. nil disables the hook entirely (CLI dry-runs, tests).
type LivenessRecorder interface {
	MarkAliveBatch(ctx context.Context, infoHashes [][]byte, observedAt time.Time, source string) (int64, error)
	RecordSuspectBatch(ctx context.Context, infoHashes [][]byte, observedAt time.Time) (int64, error)
}

// Stats summarizes one RunBatch: how the scraped hashes classified and how many
// surfacing rows changed.
type Stats struct {
	Selected        int
	Positive        int // live swarm found (seeders/leechers > 0)
	KnownZero       int // a tracker knows the hash but the swarm is dead
	Unknown         int // no tracker in the pool knows the hash
	SourcesUpserted int // authoritative 'tracker' source rows written
	SourcesCleared  int // stale 'tracker' source rows removed
	DenormSynced    int // torrent_contents rows whose seeders/leechers were refreshed
	LivenessRevived int // suspect/dead liveness rows flipped alive by a positive scrape
	LivenessSuspect int // suspect observations recorded from authoritative zeros
}

// Runner is the shared select→scrape→persist engine used by both the periodic
// worker and the refresh-seeds CLI command.
type Runner struct {
	cfg      Config
	store    *Store
	scraper  *Scraper
	metrics  *Metrics
	liveness LivenessRecorder
	logger   *zap.SugaredLogger
}

// NewRunner builds a Runner. metrics may be nil (metric updates are guarded);
// livenessRec may be nil (scrape evidence is then not fed into the liveness
// ladder — CLI contexts).
func NewRunner(cfg Config, pool lazy.Lazy[*pgxpool.Pool], metrics *Metrics, livenessRec LivenessRecorder, logger *zap.SugaredLogger) *Runner {
	return &Runner{
		cfg:      cfg,
		store:    NewStore(pool),
		scraper:  NewScraper(cfg, metrics, logger),
		metrics:  metrics,
		liveness: livenessRec,
		logger:   logger,
	}
}

// RunBatch runs one select→scrape→(persist) cycle over up to BatchSize stale
// hashes. When write is false it scrapes and classifies but persists nothing
// (dry-run) — the coverage counts still tell an operator how much of the
// catalog trackers know about before any row is written.
func (r *Runner) RunBatch(ctx context.Context, write bool) (Stats, error) {
	var st Stats

	hashes, err := r.store.SelectStale(ctx, r.cfg.MinRescrapeAge, r.cfg.BatchSize)
	if err != nil {
		r.incErr("select")
		return st, err
	}
	if len(hashes) == 0 {
		return st, nil
	}
	st.Selected = len(hashes)
	if r.metrics != nil {
		r.metrics.hashesSelected.Add(float64(len(hashes)))
	}

	outcomes := r.scraper.ScrapeBatch(ctx, hashes)
	for _, h := range hashes {
		o := outcomes[string(h)]
		if o == nil {
			st.Unknown++
			continue
		}
		switch o.Class() {
		case "positive":
			st.Positive++
		case "known_zero":
			st.KnownZero++
		default:
			st.Unknown++
		}
	}
	if r.metrics != nil {
		r.metrics.positiveTotal.Add(float64(st.Positive))
		r.metrics.knownZeroTotal.Add(float64(st.KnownZero))
		r.metrics.unknownTotal.Add(float64(st.Unknown))
	}

	if !write {
		return st, nil
	}

	up, cl, perr := r.store.Persist(ctx, hashes, outcomes)
	if perr != nil {
		r.incErr("persist")
		return st, perr
	}
	st.SourcesUpserted = up
	st.SourcesCleared = cl
	if r.metrics != nil {
		r.metrics.sourcesUpserted.Add(float64(up))
		r.metrics.sourcesCleared.Add(float64(cl))
	}

	synced, serr := r.store.SyncDenormalizedCounts(ctx, hashes)
	if serr != nil {
		r.incErr("denorm")
		return st, serr
	}
	st.DenormSynced = int(synced)
	if r.metrics != nil {
		r.metrics.denormSynced.Add(float64(synced))
	}

	// Feed scrape evidence into the liveness ladder (best-effort — liveness
	// is advisory; a failure here must not fail the cycle). Positive scrapes
	// revive suspect/dead rows (real seeders exist); authoritative zeros
	// record suspect observations. Promotion to dead stays exclusively with
	// the resolver — scrape evidence NEVER calls MarkDead.
	if r.liveness != nil {
		positives, zeros := partitionLivenessEvidence(hashes, outcomes)
		now := time.Now()
		if revived, lerr := r.liveness.MarkAliveBatch(ctx, positives, now, liveness.AliveSourceTrackerScrape); lerr != nil {
			r.incErr("liveness_alive")
			r.logger.Warnw("seeds: liveness mark-alive batch", "err", lerr)
		} else {
			st.LivenessRevived = int(revived)
		}
		if suspects, lerr := r.liveness.RecordSuspectBatch(ctx, zeros, now); lerr != nil {
			r.incErr("liveness_suspect")
			r.logger.Warnw("seeds: liveness record-suspect batch", "err", lerr)
		} else {
			st.LivenessSuspect = int(suspects)
		}
		if r.metrics != nil {
			r.metrics.livenessRevived.Add(float64(st.LivenessRevived))
			r.metrics.livenessSuspect.Add(float64(st.LivenessSuspect))
		}
	}
	return st, nil
}

// partitionLivenessEvidence routes scrape outcomes to liveness evidence:
// positive swarms (the codebase's own boundary, ScrapeOutcome.Positive():
// seeders OR leechers active — an answering swarm is alive even mid-reseed)
// revive; authoritative zeros record suspect observations; tracker-unknown
// contributes nothing (honest-unknown).
func partitionLivenessEvidence(hashes [][]byte, outcomes map[string]*ScrapeOutcome) (positives, zeros [][]byte) {
	for _, h := range hashes {
		if o := outcomes[string(h)]; o != nil && o.TrackerKnown {
			if o.Positive() {
				positives = append(positives, h)
			} else {
				zeros = append(zeros, h)
			}
		}
	}
	return positives, zeros
}

// UpdateCoverageGauges refreshes the standing coverage gauges from the ledger.
// Best-effort: errors are logged, not returned.
func (r *Runner) UpdateCoverageGauges(ctx context.Context) {
	if r.metrics == nil {
		return
	}
	ledger, known, positive, err := r.store.CoverageCounts(ctx)
	if err != nil {
		r.logger.Warnw("seeds: coverage counts", "err", err)
		return
	}
	r.metrics.ledgerRows.Set(float64(ledger))
	r.metrics.trackerKnown.Set(float64(known))
	r.metrics.positiveRows.Set(float64(positive))
}

func (r *Runner) incErr(stage string) {
	if r.metrics != nil {
		r.metrics.cycleErrors.WithLabelValues(stage).Inc()
	}
}
