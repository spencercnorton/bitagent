// Package retentionfx wires the retention worker into the app.
//
// The worker is conservative by default: Enabled=false, so including
// this module is a no-op until RETENTION_ENABLED=true. Even
// when enabled, EnablePurge defaults to false and the worker only
// emits dry-run metrics. Flipping EnablePurge is a separate,
// deliberate operator action that should follow a review of the
// bitagent_retention_would_purge_total trend.
package retentionfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/retention"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"retention",
		configfx.NewConfigModule[retention.Config]("retention", retention.NewDefaultConfig()),
		fx.Provide(
			retention.NewMetrics,
			retention.New,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *retention.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
