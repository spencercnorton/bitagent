package junkpurge

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"sync"
	"time"
)

type Metrics struct {
	cycleErrorsTotal *prometheus.CounterVec
	expiredTotal     prometheus.Counter
}

func NewMetrics() *Metrics {
	return &Metrics{prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_quarantine_errors_total", Help: "Quarantine maintenance errors"}, []string{"stage"}), prometheus.NewCounter(prometheus.CounterOpts{Name: "bitagent_quarantine_expired_total", Help: "Retained snapshots marked expired"})}
}
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.cycleErrorsTotal, m.expiredTotal}
}

type purgeWorker struct {
	cfg      Config
	verdicts *verdicts.Store
	metrics  *Metrics
	logger   *zap.SugaredLogger
}
type Params struct {
	fx.In
	Config   Config
	Pool     lazy.Lazy[*pgxpool.Pool]
	Verdicts *verdicts.Store
	Metrics  *Metrics
	Logger   *zap.SugaredLogger
}

func NewMaintenance(p Params) worker.Worker {
	var cancel context.CancelFunc
	var wg sync.WaitGroup
	return worker.NewWorker("quarantine", fx.Hook{OnStart: func(context.Context) error {
		if !p.Config.MaintenanceEnabled {
			return nil
		}
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := purgeWorker{p.Config, p.Verdicts, p.Metrics, p.Logger}
			tick := time.NewTicker(time.Minute)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					pool, e := p.Pool.Get()
					if e == nil {
						w.expireQuarantine(ctx, pool)
					}
				}
			}
		}()
		return nil
	}, OnStop: func(context.Context) error {
		if cancel != nil {
			cancel()
			wg.Wait()
		}
		return nil
	}})
}
func (w *purgeWorker) expireQuarantine(ctx context.Context, pool *pgxpool.Pool) {
	days := w.cfg.QuarantineDays
	if days <= 0 {
		days = 30
	}
	expired := 0
	for expired < quarantineExpiryCycleLimit {
		limit := min(quarantineExpiryChunkSize, quarantineExpiryCycleLimit-expired)
		n, err := expireQuarantineChunk(ctx, pool, days, limit, w.verdicts != nil)
		if err != nil {
			w.metrics.cycleErrorsTotal.WithLabelValues("expire").Inc()
			w.logger.Warnw("junkpurge expire tombstone", "err", err)
			break
		}
		expired += n
		if n < limit {
			break
		}
	}
	if expired > 0 {
		w.metrics.expiredTotal.Add(float64(expired))
		w.logger.Infow("junkpurge expired — tombstoned, snapshot retained",
			"count", expired, "window_days", days)
	}
}
