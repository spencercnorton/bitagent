package llmcapturefx

import (
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// janitorWorkerKey is the worker registry key for the capture expiry janitor.
// `worker run --all` starts it; a `--keys` subset that omits it never opens
// the database for retention.
const janitorWorkerKey = "llm_evaluation_capture_janitor"

func New() fx.Option {
	return fx.Module(
		"llm_evaluation_capture",
		configfx.NewConfigModule[llmcapture.Config](
			"llm_evaluation_capture",
			llmcapture.NewDefaultConfig(),
		),
		fx.Provide(llmcapture.NewPostgresStore),
		fx.Provide(provideRecorder),
		fx.Provide(fx.Annotated{Group: "workers", Target: provideExpiryJanitorWorker}),
	)
}

func provideRecorder(
	cfg llmcapture.Config,
	privacy *evidence.Store,
	store *llmcapture.PostgresStore,
) (llmcapture.Capturer, error) {
	return llmcapture.NewRecorder(cfg, privacy, store)
}

func provideExpiryJanitorWorker(
	cfg llmcapture.Config,
	store *llmcapture.PostgresStore,
	logger *zap.SugaredLogger,
) (worker.Worker, error) {
	janitor, err := llmcapture.NewJanitor(cfg, store, logger)
	if err != nil {
		return nil, err
	}
	return worker.NewWorker(janitorWorkerKey, fx.Hook{
		OnStart: janitor.Start,
		OnStop:  janitor.Stop,
	}), nil
}
