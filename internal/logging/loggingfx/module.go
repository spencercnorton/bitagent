package loggingfx

import (
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/logging"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func New() fx.Option {
	return fx.Module(
		"logging",
		configfx.NewConfigModule[logging.Config]("log", logging.NewDefaultConfig()),
		fx.Provide(logging.New),
	)
}

func WithLogger() fx.Option {
	return fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
		l := &fxevent.ZapLogger{Logger: log.Named("fx")}
		l.UseLogLevel(zapcore.DebugLevel)

		return l
	})
}
