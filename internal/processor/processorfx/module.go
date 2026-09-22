package processorfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/processor"
	batchqueue "github.com/spencercnorton/bitagent/internal/processor/batch/queue"
	processorqueue "github.com/spencercnorton/bitagent/internal/processor/queue"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"processor",
		fx.Provide(
			processor.New,
			processorqueue.New,
			batchqueue.New,
			processor.NewDeleteMetrics,
			fx.Annotated{
				Group: "prometheus_collectors,flatten",
				Target: func(m *processor.DeleteMetrics) []prometheus.Collector {
					return m.Collectors()
				},
			},
		),
	)
}
