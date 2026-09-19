package blockingfx

import (
	"github.com/spencercnorton/bitagent/internal/blocking"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"blocking",
		fx.Provide(
			blocking.New,
		),
	)
}
