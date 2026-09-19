// Package llmstagefx wires the LLM fallback stage into the classifier
// runner chain. The stage is installed as an fx.Decorate that wraps
// whatever Runner the classifier module (and canonical-preempt
// decorator) has already produced, so the effective call order is:
//
//	canonical preempt -> LLM stage -> CEL runner
//
// In shadow mode the LLM runs and emits metrics but returns the CEL
// result unchanged. In live mode the LLM replaces the CEL result
// when confidence >= MinConfidence. Both flags default false so
// including the module is safe; see llmstage.Config for operator
// activation.
package llmstagefx

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// llmRunnerWrapper is the named-type fx provides so this module's
// wrapper composes cleanly with classifierfx's canonical wrapper.
//
// We can't use fx.Decorate at module scope (sibling modules see the
// undecorated value — see classifierfx for the same bug). Instead the
// pattern is: classifierfx provides the `lazy.Lazy[Runner]` already
// wrapped with canonical preempt; this module re-Provides under a
// dedicated tag so fx can resolve a precise build order, then a
// `fx.Decorate` at the parent (appfx) level swaps the unwrapped Runner
// for the LLM-wrapped one. The parent-scoped Decorate is the part that
// guarantees every consumer (incl. processorfx) sees the wrapped value.
//
// Implementation here just exposes a constructor for the wrapped lazy;
// appfx wires it via `fx.Decorate` at its level.
func WrapWithLLMStage(
	inner lazy.Lazy[classifier.Runner],
	cfg llmstage.Config,
	store *evidence.Store,
	metrics *llmstage.Metrics,
	logger *zap.SugaredLogger,
	capture llmcapture.Capturer,
	pool lazy.Lazy[*pgxpool.Pool],
) lazy.Lazy[classifier.Runner] {
	return lazy.New(func() (classifier.Runner, error) {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		r, err := inner.Get()
		if err != nil {
			return nil, err
		}
		return llmstage.NewStage(cfg, r, store, metrics, logger, llmstage.Admission{
			Budget: llmmatch.NewPostgresTypeCallBudget(pool), Capture: capture,
		}), nil
	})
}

func New() fx.Option {
	return fx.Module(
		"classifier_llm",
		configfx.NewConfigModule[llmstage.Config]("classifier_llm", llmstage.NewDefaultConfig()),
		fx.Provide(llmstage.NewMetrics),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *llmstage.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
