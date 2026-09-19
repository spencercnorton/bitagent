package importerfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/importer"
	"github.com/spencercnorton/bitagent/internal/importer/httpserver"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"importer",
		fx.Provide(
			httpserver.New,
			importer.New,
			importer.NewMetrics,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *importer.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
