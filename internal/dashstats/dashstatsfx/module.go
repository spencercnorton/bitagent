// Package dashstatsfx wires the dashboard-stats collector into the app.
// Enabled by default (read-only gauges the dashboard needs); set
// DASHSTATS_ENABLED=false to turn it off.
package dashstatsfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/dashstats"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"dashstats",
		configfx.NewConfigModule[dashstats.Config]("dashstats", dashstats.NewDefaultConfig()),
		fx.Provide(
			dashstats.NewMetrics,
			dashstats.New,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *dashstats.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
