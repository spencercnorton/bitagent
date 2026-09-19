package queuefx

import (
	"github.com/spencercnorton/bitagent/internal/queue/manager"
	"github.com/spencercnorton/bitagent/internal/queue/prometheus"
	"github.com/spencercnorton/bitagent/internal/queue/server"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"queue",
		fx.Provide(
			server.New,
			manager.New,
			prometheus.New,
		),
	)
}
