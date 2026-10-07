package seeds

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/evidence/liveness"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"go.uber.org/zap"
)

// LivenessRecorder is the slice of *liveness.Store the runner feeds scrape
// evidence into. nil disables the hook entirely (CLI dry-runs, tests).
type LivenessRecorder interface {
	MarkAliveBatchTx(ctx context.Context, tx pgx.Tx, infoHashes [][]byte, observedAt time.Time, source string) (int64, error)
	RecordSuspectBatchTx(ctx context.Context, tx pgx.Tx, infoHashes [][]byte, observedAt time.Time) (int64, error)
}

// Stats summarizes one RunBatch: how the scraped hashes classified and how many
// surfacing rows changed.
type Stats struct {
	Selected        int
	Denied          int // selected public hashes withheld by a later source check
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
func NewRunner(cfg Config, pool lazy.Lazy[*pgxpool.Pool], metrics *Metrics, livenessRec LivenessRecorder, logger *zap.SugaredLogger, names ...*namepolicy.Policy) *Runner {
	store := NewStore(pool, names...)
	scraper := NewScraper(cfg, metrics, logger)
	scraper.admit = func(ctx context.Context, hashes [][]byte, use func([][]byte) error) error {
		return store.withAdmission(ctx, hashes, false, func(_ pgx.Tx, allowed [][]byte) error { return use(allowed) })
	}
	return &Runner{
		cfg:      cfg,
		store:    store,
		scraper:  scraper,
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

	outcomes, err := r.scraper.ScrapeBatch(ctx, hashes)
	if err != nil {
		r.incErr("admission")
		return st, err
	}
	admitted := make([][]byte, 0, len(hashes))
	for _, h := range hashes {
		o := outcomes[string(h)]
		if o == nil {
			st.Denied++
			continue
		}
		admitted = append(admitted, h)
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

	// Recheck and commit the seed ledger, source projection, derived counts and
	// existing liveness observations together. A late denial or any error leaves
	// no partial new facts. No transition here promotes anything to dead.
	err = r.store.withAdmission(ctx, admitted, true, func(tx pgx.Tx, allowed [][]byte) error {
		st.Denied += len(admitted) - len(allowed)
		var e error
		st.SourcesUpserted, st.SourcesCleared, e = persist(ctx, tx, allowed, outcomes)
		if e != nil {
			return e
		}
		ct, e := tx.Exec(ctx, denormSyncSQL, allowed)
		if e != nil {
			return e
		}
		st.DenormSynced = int(ct.RowsAffected())
		if r.liveness != nil {
			positives, zeros := partitionLivenessEvidence(allowed, outcomes)
			now := time.Now()
			revived, e := r.liveness.MarkAliveBatchTx(ctx, tx, positives, now, liveness.AliveSourceTrackerScrape)
			if e != nil {
				return e
			}
			st.LivenessRevived = int(revived)
			suspects, e := r.liveness.RecordSuspectBatchTx(ctx, tx, zeros, now)
			if e != nil {
				return e
			}
			st.LivenessSuspect = int(suspects)
		}
		return nil
	})
	if err != nil {
		r.incErr("persist")
		st.SourcesUpserted = 0
		st.SourcesCleared = 0
		st.DenormSynced = 0
		st.LivenessRevived = 0
		st.LivenessSuspect = 0
		return st, err
	}
	if r.metrics != nil {
		r.metrics.sourcesUpserted.Add(float64(st.SourcesUpserted))
		r.metrics.sourcesCleared.Add(float64(st.SourcesCleared))
		r.metrics.denormSynced.Add(float64(st.DenormSynced))
		r.metrics.livenessRevived.Add(float64(st.LivenessRevived))
		r.metrics.livenessSuspect.Add(float64(st.LivenessSuspect))
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
