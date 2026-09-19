package telemetryfx

import (
	"github.com/spencercnorton/bitagent/internal/telemetry/httpserver"
	"github.com/spencercnorton/bitagent/internal/telemetry/prometheus"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"telemetry",
		fx.Provide(
			httpserver.New,
			prometheus.New,
		),
	)
}
