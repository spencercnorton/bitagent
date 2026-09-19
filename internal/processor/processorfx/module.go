package processorfx

import (
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
		),
	)
}
