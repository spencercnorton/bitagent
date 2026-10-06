// Package llmmatchfx wires the two-stage LLM TMDB matcher into the classifier.
// Unlike llmstagefx (a Runner decorator), this feature is a workflow ACTION —
// so the module only needs to provide the *llmmatch.Client into the DI graph;
// classifier.New picks it up as an optional param and hands it to the action.
// Both config flags default false, so including the module is inert until an
// operator opts in.
package llmmatchfx

import (
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func New() fx.Option {
	return fx.Module(
		"classifier_llm_match",
		configfx.NewConfigModule[llmmatch.Config]("classifier_llm_match", llmmatch.NewDefaultConfig()),
		fx.Provide(llmmatch.NewMetrics),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *llmmatch.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
		fx.Provide(provideClient),
	)
}

// provideClient builds the matcher client. The evidence store satisfies the
// privacy gate (never sends private-tracker names to the LLM).
type clientParams struct {
	fx.In
	Config   llmmatch.Config
	Store    *evidence.Store
	Metrics  *llmmatch.Metrics
	Logger   *zap.SugaredLogger
	Capture  llmcapture.Capturer `optional:"true"`
	Pool     lazy.Lazy[*pgxpool.Pool]
	Policy   classifier.Config          `optional:"true"`
	Work     *llmwork.Store             `optional:"true"`
	Dispatch llmcapture.DispatchControl `optional:"true"`
}

func provideClient(p clientParams) (*llmmatch.Client, error) {
	if err := p.Config.Validate(); err != nil {
		return nil, err
	}
	if p.Config.Enabled && p.Config.Embeddings.Enabled {
		if p.Capture == nil || !p.Capture.Enabled() {
			return nil, fmt.Errorf("embeddings requires enabled evaluation capture")
		}
		if _, ok := p.Capture.(llmcapture.EmbeddingResultRecorder); !ok {
			return nil, fmt.Errorf("embeddings requires source-bound admission rechecks")
		}
		if _, ok := p.Capture.(llmcapture.ResultRecorder); !ok {
			return nil, fmt.Errorf("embeddings requires final match decision recording")
		}
	}
	client := llmmatch.NewClientWithBudget(
		p.Config,
		p.Store,
		p.Metrics,
		p.Logger,
		p.Capture,
		llmmatch.NewPostgresCallBudget(p.Pool),
	).WithDispatchControl(p.Dispatch)
	client.SetWork(p.Work, p.Policy)
	return client, nil
}
