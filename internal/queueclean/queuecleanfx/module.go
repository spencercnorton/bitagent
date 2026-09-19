// Package queuecleanfx wires the queueclean worker into the app.
//
// Conservative by default: QUEUECLEAN_ENABLED=false makes everything
// a pure no-op. Including this module is safe even before the operator
// has decided on retention; the worker simply doesn't fire.
//
// What it provides:
//   - configfx section "queueclean" (env prefix QUEUECLEAN_*)
//   - *queueclean.Metrics
//   - prometheus_collectors fx-group entries
//   - a worker entry that runs on the configured Interval
package queuecleanfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/queueclean"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"queueclean",
		configfx.NewConfigModule[queueclean.Config]("queueclean", queueclean.NewDefaultConfig()),
		fx.Provide(
			queueclean.NewMetrics,
			queueclean.New,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *queueclean.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
