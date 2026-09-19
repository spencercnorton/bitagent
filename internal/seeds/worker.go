package seeds

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/evidence/liveness"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const workerKey = "seeds"

type Params struct {
	fx.In
	Config   Config
	Pool     lazy.Lazy[*pgxpool.Pool]
	Metrics  *Metrics
	Liveness *liveness.Store `optional:"true"`
	Logger   *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

func New(p Params) Result {
	// typed-nil guard: a nil *liveness.Store wrapped in the interface would
	// defeat the runner's nil check and panic on first use.
	var livenessRec LivenessRecorder
	if p.Liveness != nil {
		livenessRec = p.Liveness
	}
	w := &seedsWorker{
		cfg:     p.Config,
		runner:  NewRunner(p.Config, p.Pool, p.Metrics, livenessRec, p.Logger.Named("seeds")),
		metrics: p.Metrics,
		logger:  p.Logger.Named("seeds"),
	}
	return Result{Worker: worker.NewWorker(workerKey, fx.Hook{
		OnStart: w.start,
		OnStop:  w.stop,
	})}
}

type seedsWorker struct {
	cfg     Config
	runner  *Runner
	metrics *Metrics
	logger  *zap.SugaredLogger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (w *seedsWorker) start(context.Context) error {
	if !w.cfg.Enabled {
		w.logger.Info("seeds disabled via config; skipping start")
		return nil
	}
	if w.cfg.Interval <= 0 || w.cfg.BatchSize <= 0 || w.cfg.MaxHashesPerPacket <= 0 {
		return errors.New("seeds: interval, batch_size and max_hashes_per_packet must be positive when enabled")
	}
	if len(w.cfg.TrackerUrls) == 0 {
		return errors.New("seeds: tracker_urls must be non-empty when enabled")
	}
	if !w.cfg.EnableWrite {
		w.logger.Warn("seeds: EnableWrite is false — running in dry-run (scrape + metrics, no DB writes)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go w.loop(ctx)
	return nil
}

func (w *seedsWorker) stop(context.Context) error {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	return nil
}

func (w *seedsWorker) loop(ctx context.Context) {
	defer w.wg.Done()

	// Offset the first cycle so we don't slam the DB and the tracker pool at
	// boot alongside the other workers.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
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

func (w *seedsWorker) runCycle(ctx context.Context) {
	start := time.Now()
	st, err := w.runner.RunBatch(ctx, w.cfg.EnableWrite)
	if err != nil {
		w.logger.Warnw("seeds: cycle error", "err", err)
		return
	}
	if w.metrics != nil {
		w.metrics.cyclesTotal.Inc()
		w.metrics.cycleDuration.Observe(time.Since(start).Seconds())
	}
	w.runner.UpdateCoverageGauges(ctx)

	if st.Selected == 0 {
		w.logger.Debug("seeds: no stale hashes this cycle")
		return
	}
	w.logger.Infow("seeds cycle complete",
		"mode", writeMode(w.cfg.EnableWrite),
		"selected", st.Selected,
		"positive", st.Positive,
		"known_zero", st.KnownZero,
		"unknown", st.Unknown,
		"sources_upserted", st.SourcesUpserted,
		"sources_cleared", st.SourcesCleared,
		"denorm_synced", st.DenormSynced,
		"liveness_revived", st.LivenessRevived,
		"liveness_suspect", st.LivenessSuspect,
		"duration", time.Since(start).String(),
	)
}

func writeMode(write bool) string {
	if write {
		return "LIVE"
	}
	return "DRY-RUN"
}
