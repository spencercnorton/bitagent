package metricsfx

import (
	"github.com/spencercnorton/bitagent/internal/metrics/queuemetrics"
	"github.com/spencercnorton/bitagent/internal/metrics/torrentmetrics"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"queue",
		fx.Provide(
			queuemetrics.New,
			torrentmetrics.New,
		),
	)
}
