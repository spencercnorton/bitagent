// Package pgstatsfx wires the pgstats collector into the prometheus
// collector group. Registering the module is enough to start reporting
// Postgres health metrics at /metrics; no further configuration required.
package pgstatsfx

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/database/pgstats"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Pool   lazy.Lazy[*pgxpool.Pool]
	Logger *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Collector prometheus.Collector `group:"prometheus_collectors"`
}

func New() fx.Option {
	return fx.Module(
		"pgstats",
		fx.Provide(func(p Params) Result {
			return Result{Collector: pgstats.NewCollector(p.Pool, p.Logger)}
		}),
	)
}
