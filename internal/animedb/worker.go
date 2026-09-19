package animedb

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const workerKey = "anime-titles"

type Params struct {
	fx.In
	Config  Config
	Runner  *Runner
	Store   *Store
	Metrics *Metrics
	Logger  *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

// New provides the scheduled anime-titles refresh worker. Off by default; enable
// with ANIME_TITLES_ENABLED=true and, after reviewing dry-run build counts,
// ANIME_TITLES_ENABLE_WRITE=true.
func New(p Params) Result {
	w := &refreshWorker{
		cfg:     p.Config,
		runner:  p.Runner,
		store:   p.Store,
		metrics: p.Metrics,
		logger:  p.Logger.Named("animedb"),
	}
	return Result{Worker: worker.NewWorker(workerKey, fx.Hook{
		OnStart: w.start,
		OnStop:  w.stop,
	})}
}

type refreshWorker struct {
	cfg     Config
	runner  *Runner
	store   *Store
	metrics *Metrics
	logger  *zap.SugaredLogger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (w *refreshWorker) start(context.Context) error {
	if !w.cfg.Enabled {
		w.logger.Info("animedb disabled via config; skipping start (resolver still serves seed + persisted table)")
		return nil
	}
	if w.cfg.Interval <= 0 {
		return errors.New("animedb: interval must be positive when enabled")
	}
	if w.cfg.AnimeListUrl == "" || w.cfg.AnidbTitlesUrl == "" {
		return errors.New("animedb: anime_list_url and anidb_titles_url must be set when enabled")
	}
	if !w.cfg.EnableWrite {
		w.logger.Warn("animedb: enable_write is false — refreshing in dry-run (download + build + metrics, no DB writes)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go w.loop(ctx)
	return nil
}

func (w *refreshWorker) stop(context.Context) error {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	return nil
}

func (w *refreshWorker) loop(ctx context.Context) {
	defer w.wg.Done()

	// Offset the first cycle so a boot doesn't download alongside every other
	// worker warming up.
	select {
	case <-ctx.Done():
		return
	case <-time.After(60 * time.Second):
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

func (w *refreshWorker) runCycle(ctx context.Context) {
	// In write mode, skip the network entirely if the persisted table is still
	// fresh — protects the upstream hosts (AniDB asks for at most one fetch/day)
	// across restarts. Dry-run always runs so an operator can measure on demand.
	if w.cfg.EnableWrite && w.cfg.MinRefreshAge > 0 {
		if ts, ok, err := w.store.LastUpdated(ctx); err == nil && ok {
			if age := time.Since(ts); age < w.cfg.MinRefreshAge {
				w.logger.Debugw("animedb: table still fresh; skipping cycle", "age", age.String())
				return
			}
		}
	}

	stats, err := w.runner.Refresh(ctx, w.cfg.EnableWrite)
	if err != nil {
		w.logger.Warnw("animedb: refresh error", "err", err)
		return
	}
	w.logger.Infow("animedb refresh complete",
		"mode", refreshMode(w.cfg.EnableWrite),
		"mappings", stats.Mappings,
		"titles", stats.Titles,
		"aliases", stats.Aliases,
		"rows_written", stats.Rows,
	)
}

func refreshMode(write bool) string {
	if write {
		return "LIVE"
	}
	return "DRY-RUN"
}
