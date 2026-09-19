package llmcapturefx

import (
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func New() fx.Option {
	return fx.Module(
		"llm_evaluation_capture",
		configfx.NewConfigModule[llmcapture.Config](
			"llm_evaluation_capture",
			llmcapture.NewDefaultConfig(),
		),
		fx.Provide(llmcapture.NewPostgresStore),
		fx.Provide(provideRecorder),
		fx.Invoke(registerExpiryJanitor),
	)
}

func provideRecorder(
	cfg llmcapture.Config,
	privacy *evidence.Store,
	store *llmcapture.PostgresStore,
) (llmcapture.Capturer, error) {
	return llmcapture.NewRecorder(cfg, privacy, store)
}

func registerExpiryJanitor(
	lifecycle fx.Lifecycle,
	cfg llmcapture.Config,
	store *llmcapture.PostgresStore,
	logger *zap.SugaredLogger,
) error {
	janitor, err := llmcapture.NewJanitor(cfg, store, logger)
	if err != nil {
		return err
	}
	lifecycle.Append(fx.Hook{
		OnStart: janitor.Start,
		OnStop:  janitor.Stop,
	})
	return nil
}
