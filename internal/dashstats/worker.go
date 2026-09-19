package dashstats

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const workerKey = "dashstats"

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

func New(p Params) Result {
	w := &statsWorker{
		cfg:     p.Config,
		pool:    p.Pool,
		metrics: p.Metrics,
		logger:  p.Logger.Named("dashstats"),
	}
	return Result{Worker: worker.NewWorker(workerKey, fx.Hook{
		OnStart: w.start,
		OnStop:  w.stop,
	})}
}

type statsWorker struct {
	cfg     Config
	pool    lazy.Lazy[*pgxpool.Pool]
	metrics *Metrics
	logger  *zap.SugaredLogger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (w *statsWorker) start(context.Context) error {
	if !w.cfg.Enabled {
		w.logger.Info("dashstats disabled via config; skipping start")
		return nil
	}
	if w.cfg.Interval <= 0 {
		return errors.New("dashstats: interval must be positive when enabled")
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go w.loop(ctx)
	return nil
}

func (w *statsWorker) stop(context.Context) error {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	return nil
}

func (w *statsWorker) loop(ctx context.Context) {
	defer w.wg.Done()
	// Refresh once promptly so the dashboard has values soon after boot,
	// then on the configured interval.
	w.refresh(ctx)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.refresh(ctx)
		}
	}
}

const grabQuery = `
SELECT
  count(*) FILTER (WHERE outcome = 'success'),
  count(*) FILTER (WHERE outcome = 'failure'),
  count(*) FILTER (WHERE resolved_at IS NULL)
FROM torrent_grab_attempts`

// matchQuery counts movie/tv torrents added in the last 30 days and how many
// got a content match (content_id present). Music/ebook/audiobook are excluded
// — they have no metadata source to match against, so including them would
// dilute the figure (operator decision 2026-06-26).
const matchQuery = `
SELECT
  count(*),
  count(*) FILTER (WHERE content_id IS NOT NULL)
FROM torrent_contents
WHERE content_type IN ('movie', 'tv_show')
  AND created_at > now() - interval '30 days'`

// altTitleQuery measures alt-title backfill coverage over the rows
// refresh-alt-titles targets (tmdb movie/tv_show/xxx). "checked" counts rows
// the sweep has already visited — the alt_titles_checked marker or any
// alt_title:* attribute — so rows with zero upstream alt titles still count
// as covered; "with_alt" is the subset that actually gained alt titles. The
// underscore in the LIKE pattern is escaped so it matches literally. EXISTS
// probes ride the content_attributes primary key.
const altTitleQuery = `
SELECT
  count(*),
  count(*) FILTER (WHERE EXISTS (
    SELECT 1 FROM content_attributes ca
    WHERE ca.content_type = c.type AND ca.content_source = c.source AND ca.content_id = c.id
      AND (ca.key = 'alt_titles_checked' OR ca.key LIKE 'alt\_title:%')
  )),
  count(*) FILTER (WHERE EXISTS (
    SELECT 1 FROM content_attributes ca
    WHERE ca.content_type = c.type AND ca.content_source = c.source AND ca.content_id = c.id
      AND ca.key LIKE 'alt\_title:%'
  ))
FROM content c
WHERE c.type IN ('movie', 'tv_show', 'xxx')
  AND c.source = 'tmdb'`

// indexerGrabQuery counts *arr grab webhooks over the last 30 days and the
// subset won by this instance's own indexer (raw_payload->release->indexer,
// substring-matched so a rename keeps counting). Same shape as the GraphQL
// evidence.indexerStats aggregate; raw counts, the dashboard frames the ratio.
const indexerGrabQuery = `
SELECT
  count(*),
  count(*) FILTER (WHERE raw_payload->'release'->>'indexer' ILIKE '%bitagent%')
FROM label_evidence
WHERE source_kind = 'webhook_grab'
  AND observed_at > now() - interval '30 days'`

func (w *statsWorker) refresh(ctx context.Context) {
	pool, err := w.pool.Get()
	if err != nil {
		w.logger.Warnw("dashstats acquire pool", "err", err)
		return
	}

	var success, failure, pending int64
	if err := pool.QueryRow(ctx, grabQuery).Scan(&success, &failure, &pending); err != nil {
		w.logger.Warnw("dashstats grab query", "err", err)
	} else {
		w.metrics.grabSuccess.Set(float64(success))
		w.metrics.grabFailure.Set(float64(failure))
		w.metrics.grabPending.Set(float64(pending))
	}

	var total, matched int64
	if err := pool.QueryRow(ctx, matchQuery).Scan(&total, &matched); err != nil {
		w.logger.Warnw("dashstats match query", "err", err)
	} else {
		w.metrics.matchVideoTotal.Set(float64(total))
		w.metrics.matchVideoMatched.Set(float64(matched))
	}

	var altTotal, altChecked, altWithAlt int64
	if err := pool.QueryRow(ctx, altTitleQuery).Scan(&altTotal, &altChecked, &altWithAlt); err != nil {
		w.logger.Warnw("dashstats alt-title query", "err", err)
	} else {
		w.metrics.altTitleTotal.Set(float64(altTotal))
		w.metrics.altTitleChecked.Set(float64(altChecked))
		w.metrics.altTitleWithAlt.Set(float64(altWithAlt))
	}

	var grabsTotal, grabsBitagent int64
	if err := pool.QueryRow(ctx, indexerGrabQuery).Scan(&grabsTotal, &grabsBitagent); err != nil {
		w.logger.Warnw("dashstats indexer-grab query", "err", err)
	} else {
		w.metrics.grabsTotal30d.Set(float64(grabsTotal))
		w.metrics.grabsBitagent30d.Set(float64(grabsBitagent))
	}
}
