package app

import (
	"github.com/spencercnorton/bitagent/internal/app/appfx"
	"github.com/spencercnorton/bitagent/internal/app/cli/hooks"
	"github.com/spencercnorton/bitagent/internal/logging/loggingfx"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func New() *fx.App {
	return fx.New(
		appfx.New(),
		loggingfx.WithLogger(),
		fx.Invoke(func(
			logger *zap.SugaredLogger,
			_ *cli.App,
			_ hooks.AttachedHooks,
		) {
			logger.Debug("app invoked")
		}),
	)
}
