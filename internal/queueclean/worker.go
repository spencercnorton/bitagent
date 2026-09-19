package queueclean

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const workerKey = "queueclean"

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

// New wires the queueclean worker. Respects Config.Enabled — when
// false, OnStart is a no-op so the scheduled cycles never fire.
func New(p Params) Result {
	w := &cleanWorker{
		cfg:     p.Config,
		pool:    p.Pool,
		metrics: p.Metrics,
		logger:  p.Logger.Named("queueclean"),
	}
	return Result{Worker: worker.NewWorker(workerKey, fx.Hook{
		OnStart: w.start,
		OnStop:  w.stop,
	})}
}

type cleanWorker struct {
	cfg     Config
	pool    lazy.Lazy[*pgxpool.Pool]
	metrics *Metrics
	logger  *zap.SugaredLogger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (w *cleanWorker) start(context.Context) error {
	if !w.cfg.Enabled {
		w.logger.Info("queueclean disabled via config; skipping start")
		return nil
	}
	if w.cfg.Interval <= 0 || w.cfg.RetentionAge <= 0 || w.cfg.BatchSize <= 0 {
		return errors.New("queueclean: interval, retention_age and batch_size must be positive when enabled")
	}
	if len(w.cfg.PurgeStatuses) == 0 {
		return errors.New("queueclean: purge_statuses cannot be empty")
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go w.loop(ctx)
	w.logger.Infow("queueclean started",
		"interval", w.cfg.Interval,
		"retention_age", w.cfg.RetentionAge,
		"batch_size", w.cfg.BatchSize,
		"purge_statuses", w.cfg.PurgeStatuses,
		"enable_purge", w.cfg.EnablePurge,
	)
	return nil
}

func (w *cleanWorker) stop(context.Context) error {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	return nil
}

func (w *cleanWorker) loop(ctx context.Context) {
	defer w.wg.Done()

	// Offset the first cycle so we don't slam the DB at startup.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
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

func (w *cleanWorker) runCycle(ctx context.Context) {
	start := time.Now()
	defer func() {
		w.metrics.cyclesTotal.Inc()
		w.metrics.cycleDuration.Observe(time.Since(start).Seconds())
		w.metrics.lastCycleUnix.Set(float64(time.Now().Unix()))
	}()

	pool, err := w.pool.Get()
	if err != nil {
		w.metrics.cycleErrorsTotal.WithLabelValues("query").Inc()
		w.logger.Warnw("queueclean acquire pool", "err", err)
		return
	}

	// Per-status candidate count (the planner uses the partial index
	// path on (queue, status) — same predicate the rest of the queue
	// system uses, so we don't slow anything down).
	cutoff := time.Now().Add(-w.cfg.RetentionAge)
	for _, status := range w.cfg.PurgeStatuses {
		count, cErr := w.countCandidates(ctx, pool, status, cutoff)
		if cErr != nil {
			w.metrics.cycleErrorsTotal.WithLabelValues("query").Inc()
			w.logger.Warnw("queueclean count candidates", "status", status, "err", cErr)
			continue
		}
		w.metrics.candidatesTotal.WithLabelValues(status).Add(float64(count))

		batch := count
		if batch > int64(w.cfg.BatchSize) {
			batch = int64(w.cfg.BatchSize)
		}
		if batch == 0 {
			continue
		}

		if !w.cfg.EnablePurge {
			w.metrics.wouldPurgeTotal.WithLabelValues(status).Add(float64(batch))
			w.logger.Infow("queueclean dry-run",
				"status", status,
				"candidates_total", count,
				"would_purge_this_cycle", batch,
				"cutoff", cutoff,
			)
			continue
		}

		purged, pErr := w.purgeBatch(ctx, pool, status, cutoff)
		if pErr != nil {
			w.metrics.cycleErrorsTotal.WithLabelValues("delete").Inc()
			w.logger.Errorw("queueclean purge", "status", status, "err", pErr)
			continue
		}
		w.metrics.purgedTotal.WithLabelValues(status).Add(float64(purged))
		w.logger.Infow("queueclean purge complete",
			"status", status, "purged", purged, "candidates_total", count)
	}
}

// countCandidates is intentionally simple: queue_jobs is small enough
// (~14k rows live) that a count() with two-column predicate is sub-ms.
func (w *cleanWorker) countCandidates(ctx context.Context, pool *pgxpool.Pool, status string, cutoff time.Time) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM queue_jobs WHERE status = $1 AND created_at < $2`,
		status, cutoff,
	).Scan(&n)
	return n, err
}

// purgeBatch deletes BatchSize rows max per call. The id IN (subquery)
// pattern lets Postgres use the (queue, status) index for the inner
// scan and avoid locking rows we're not deleting.
func (w *cleanWorker) purgeBatch(ctx context.Context, pool *pgxpool.Pool, status string, cutoff time.Time) (int64, error) {
	q := `
DELETE FROM queue_jobs
WHERE id IN (
  SELECT id FROM queue_jobs
  WHERE status = $1 AND created_at < $2
  ORDER BY created_at ASC
  LIMIT $3
)`
	tag, err := pool.Exec(ctx, q, status, cutoff, w.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// statusListCSV is here for log readability; not used in SQL paths.
func statusListCSV(s []string) string {
	return strings.Join(s, ",")
}

// fmtCutoff is here for log readability; pgx's default time formatting
// is verbose.
func fmtCutoff(t time.Time) string {
	return fmt.Sprintf("%s UTC", t.UTC().Format(time.RFC3339))
}

var _ = statusListCSV // keep helper for future log enrichment
var _ = fmtCutoff     // keep helper for future log enrichment
