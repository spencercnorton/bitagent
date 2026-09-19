package retention

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const workerKey = "retention"

type Params struct {
	fx.In
	Config  Config
	Pool    lazy.Lazy[*pgxpool.Pool]
	Metrics *Metrics
	Logger  *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

// New wires the retention worker. It respects Config.Enabled — when
// false the Worker's OnStart is a no-op so the scheduled cycles
// never fire.
func New(p Params) Result {
	w := &retentionWorker{
		cfg:     p.Config,
		pool:    p.Pool,
		metrics: p.Metrics,
		logger:  p.Logger.Named("retention"),
	}
	return Result{Worker: worker.NewWorker(workerKey, fx.Hook{
		OnStart: w.start,
		OnStop:  w.stop,
	})}
}

type retentionWorker struct {
	cfg     Config
	pool    lazy.Lazy[*pgxpool.Pool]
	metrics *Metrics
	logger  *zap.SugaredLogger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (w *retentionWorker) start(context.Context) error {
	if !w.cfg.Enabled {
		w.logger.Info("retention disabled via config; skipping start")
		return nil
	}
	if w.cfg.Interval <= 0 || w.cfg.MinAge <= 0 || w.cfg.MaxLastSeen <= 0 || w.cfg.BatchSize <= 0 || w.cfg.SourceFreshnessMaxAge <= 0 {
		return errors.New("retention: interval, min_age, max_last_seen, batch_size and source_freshness_max_age must be positive when enabled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go w.loop(ctx)
	return nil
}

func (w *retentionWorker) stop(context.Context) error {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	return nil
}

func (w *retentionWorker) loop(ctx context.Context) {
	defer w.wg.Done()

	// Offset the first cycle so we don't slam the DB at startup
	// alongside the other workers.
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Minute):
	}

	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()

	w.runCycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.runCycle(ctx)
		}
	}
}

func (w *retentionWorker) runCycle(ctx context.Context) {
	start := time.Now()
	defer func() {
		w.metrics.cyclesTotal.Inc()
		w.metrics.cycleDuration.Observe(time.Since(start).Seconds())
		w.metrics.lastCycleUnix.Set(float64(time.Now().Unix()))
	}()

	pool, err := w.pool.Get()
	if err != nil {
		w.metrics.cycleErrorsTotal.WithLabelValues("query").Inc()
		w.logger.Warnw("retention acquire pool", "err", err)
		return
	}

	// Phase 1: count candidates (the predicate without cap). This
	// is the denominator for the dry-run signal.
	total, err := countCandidates(ctx, pool, w.cfg)
	if err != nil {
		w.metrics.cycleErrorsTotal.WithLabelValues("query").Inc()
		w.logger.Warnw("retention count candidates", "err", err)
		return
	}
	w.metrics.candidatesTotal.Add(float64(total))

	// Phase 2: apply cap and either record or delete.
	batch := int(min64(int64(w.cfg.BatchSize), total))
	if batch == 0 {
		return
	}

	if !w.cfg.EnablePurge {
		w.metrics.wouldPurgeTotal.Add(float64(batch))
		w.logger.Infow("retention dry-run",
			"candidates_total", total,
			"would_purge_this_cycle", batch,
			"threshold_last_seen_before", time.Now().Add(-w.cfg.MaxLastSeen),
			"threshold_created_before", time.Now().Add(-w.cfg.MinAge),
		)
		return
	}

	purged, err := purgeBatch(ctx, pool, w.cfg)
	if err != nil {
		w.metrics.cycleErrorsTotal.WithLabelValues("delete").Inc()
		w.logger.Errorw("retention purge batch", "err", err)
		return
	}
	w.metrics.purgedTotal.Add(float64(purged))
	w.logger.Infow("retention purge complete",
		"purged", purged, "candidates_total", total)
}

// predicate is the retention filter. A row enters the candidate set
// when ALL hold:
//
//  1. no canonical label exists for its info_hash
//  2. no label_evidence row exists for its info_hash
//  3. it has been in the DB at least cfg.MinAge
//  4. torrents.updated_at is older than cfg.MaxLastSeen
//  5. there exists at least one torrents_torrent_sources row that
//     reports a *recent, observed* zero-seeders state — i.e.
//     seeders IS NOT NULL AND seeders = 0 AND updated_at >= $3
//     (= now - cfg.SourceFreshnessMaxAge).
//  6. NO torrents_torrent_sources row indicates "alive or unknown":
//     seeders > 0, seeders IS NULL, OR updated_at < $3.
//
// Conditions 5 and 6 together encode the GPT-5.5-pro review note:
// `COALESCE(seeders,0)=0` was conflating observed-zero, never-measured,
// and stale-zero into a single "dead" verdict — and silently purging
// torrents whose tracker happened to be offline. The hardened
// predicate keeps NULL or stale source rows on the "alive or unknown"
// side and requires at least one fresh, explicit zero before purge.
//
// Classifier confidence is NOT a keep signal — only evidence-layer
// presence and observed-fresh-zero spare or condemn a row.
//
// SQL parameters:
//
//	$1 = now - cfg.MinAge                  (created_at upper bound)
//	$2 = now - cfg.MaxLastSeen             (updated_at upper bound)
//	$3 = now - cfg.SourceFreshnessMaxAge   (TTS freshness boundary)
const predicate = `
  NOT EXISTS (SELECT 1 FROM torrent_canonical_labels l WHERE l.info_hash = t.info_hash)
  AND NOT EXISTS (SELECT 1 FROM label_evidence e      WHERE e.info_hash = t.info_hash)
  AND t.created_at < $1
  AND t.updated_at < $2
  AND EXISTS (
    SELECT 1 FROM torrents_torrent_sources s
    WHERE s.info_hash = t.info_hash
      AND s.seeders IS NOT NULL
      AND s.seeders = 0
      AND s.updated_at >= $3
  )
  AND NOT EXISTS (
    SELECT 1 FROM torrents_torrent_sources s
    WHERE s.info_hash = t.info_hash
      AND (
        s.seeders IS NULL
        OR s.seeders > 0
        OR s.updated_at < $3
      )
  )
`

func countCandidates(ctx context.Context, pool *pgxpool.Pool, cfg Config) (int64, error) {
	q := fmt.Sprintf(`SELECT count(*) FROM torrents t WHERE %s`, predicate)
	var n int64
	err := pool.QueryRow(ctx,
		q,
		time.Now().Add(-cfg.MinAge),
		time.Now().Add(-cfg.MaxLastSeen),
		time.Now().Add(-cfg.SourceFreshnessMaxAge),
	).Scan(&n)
	return n, err
}

func purgeBatch(ctx context.Context, pool *pgxpool.Pool, cfg Config) (int64, error) {
	q := fmt.Sprintf(`
DELETE FROM torrents
WHERE info_hash IN (
    SELECT t.info_hash FROM torrents t
    WHERE %s
    ORDER BY t.updated_at ASC
    LIMIT $4
)`, predicate)
	tag, err := pool.Exec(ctx,
		q,
		time.Now().Add(-cfg.MinAge),
		time.Now().Add(-cfg.MaxLastSeen),
		time.Now().Add(-cfg.SourceFreshnessMaxAge),
		cfg.BatchSize,
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
