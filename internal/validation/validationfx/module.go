package validationfx

import (
	"github.com/spencercnorton/bitagent/internal/validation"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"validation",
		fx.Provide(validation.New),
	)
}
