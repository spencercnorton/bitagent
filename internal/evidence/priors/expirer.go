package priors

import (
	"context"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	expirerWorkerKey = "evidence_priors_expirer"
	// expireBatchSize caps how many pending grabs are resolved in a
	// single transaction. Each batch is one round trip; keeping the
	// number small bounds lock duration if Postgres is busy.
	expireBatchSize = 200
	// purgeInterval is how often resolved torrent_grab_attempts rows
	// older than PurgeResolvedAfterDays are removed. Independent of
	// ExpirerInterval because purge is housekeeping, not learning.
	purgeInterval = 6 * time.Hour
)

// ExpirerParams groups the fx dependencies of the expirer.
type ExpirerParams struct {
	fx.In
	Config   evidence.Config
	Store    *Store
	Resolver *Resolver
	Metrics  *Metrics
	Logger   *zap.SugaredLogger
}

// ExpirerResult exports the worker into the workers group so the
// app's worker runner starts it on boot.
type ExpirerResult struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

// NewExpirer wires the expirer worker. When priors are disabled the
// worker is registered but its run loop returns immediately on start,
// so registration is cheap.
func NewExpirer(p ExpirerParams) ExpirerResult {
	e := &expirer{
		cfg:      p.Config.OutcomePriors,
		store:    p.Store,
		resolver: p.Resolver,
		metrics:  p.Metrics,
		logger:   p.Logger.Named("priors-expirer"),
		now:      time.Now,
	}
	return ExpirerResult{Worker: worker.NewWorker(expirerWorkerKey, fx.Hook{
		OnStart: e.start,
		OnStop:  e.stop,
	})}
}

type expirer struct {
	cfg      evidence.OutcomePriorsConfig
	store    *Store
	resolver *Resolver
	metrics  *Metrics
	logger   *zap.SugaredLogger
	now      func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (e *expirer) start(context.Context) error {
	if !e.cfg.Enabled {
		e.logger.Info("priors disabled; expirer dormant")
		return nil
	}
	if err := validateConfig(e.cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.wg.Add(2)
	go e.expireLoop(ctx)
	go e.purgeLoop(ctx)
	return nil
}

func (e *expirer) stop(context.Context) error {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
	return nil
}

func (e *expirer) expireLoop(ctx context.Context) {
	defer e.wg.Done()

	// Initial cycle on start to handle rows that built up while the
	// process was down.
	e.cycle(ctx)

	tick := time.NewTicker(e.cfg.ExpirerInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.cycle(ctx)
		}
	}
}

func (e *expirer) cycle(ctx context.Context) {
	cutoff := e.now().Add(-e.cfg.ResolutionWindow)
	for {
		expired, err := e.store.ExpirePending(ctx, cutoff, e.now(), expireBatchSize)
		if err != nil {
			e.logger.Warnw("priors expirer cycle", "err", err)
			return
		}
		if len(expired) == 0 {
			break
		}
		e.resolver.ApplyExpired(ctx, expired)
		e.metrics.Expired(len(expired))
		if len(expired) < expireBatchSize {
			break
		}
	}
	if pending, err := e.store.CountPending(ctx); err == nil {
		e.metrics.SetPending(float64(pending))
	}
}

func (e *expirer) purgeLoop(ctx context.Context) {
	defer e.wg.Done()
	if e.cfg.PurgeResolvedAfterDays <= 0 {
		return
	}
	tick := time.NewTicker(purgeInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			cutoff := e.now().Add(-e.cfg.PurgeResolvedAfter())
			if _, err := e.store.PurgeResolved(ctx, cutoff); err != nil {
				e.logger.Warnw("priors expirer purge", "err", err)
			}
		}
	}
}
