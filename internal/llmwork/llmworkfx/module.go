// Package llmworkfx wires the disabled-by-default optional model task queue.
package llmworkfx

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module("llm_work",
		configfx.NewConfigModule[llmwork.Config]("llm_work", llmwork.NewDefaultConfig()),
		fx.Provide(provideStore),
		fx.Provide(fx.Annotated{Group: "prometheus_collectors,flatten", Target: func(store *llmwork.Store) []prometheus.Collector { return store.Metrics().Collectors() }}),
	)
}

type storeParams struct {
	fx.In
	Config     llmwork.Config
	Pool       lazy.Lazy[*pgxpool.Pool]
	Capture    llmcapture.Capturer
	Dispatch   llmcapture.DispatchControl
	Classifier classifier.Config
	Type       llmstage.Config
	Language   contentfilter.Config
	Matcher    llmmatch.Config
}

func provideStore(p storeParams) (*llmwork.Store, error) {
	if p.Config.Enabled {
		if p.Capture == nil || !p.Capture.Enabled() || p.Dispatch == nil || !p.Dispatch.Enabled() {
			return nil, fmt.Errorf("llm_work requires enabled durable capture and dispatch control")
		}
		if p.Config.Accepts(llmwork.Type) && (!p.Type.Enabled || p.Classifier.Workflow != "default") {
			return nil, fmt.Errorf("llm_work classifier_type requires enabled type inference and the default policy adapter")
		}
		if p.Config.Accepts(llmwork.Type) {
			if err := p.Type.Validate(); err != nil {
				return nil, err
			}
		}
		if p.Config.Accepts(llmwork.Language) && (!p.Language.Enabled || !p.Language.LLMEnabled || p.Language.EffectiveLLMAction() != contentfilter.LLMActionReview) {
			return nil, fmt.Errorf("llm_work contentfilter requires enabled review-only language inference")
		}
		if p.Config.Accepts(llmwork.Matcher) && !p.Matcher.Enabled {
			return nil, fmt.Errorf("llm_work matcher requires enabled identity inference")
		}
		if p.Config.Accepts(llmwork.Matcher) {
			if err := p.Matcher.Validate(); err != nil {
				return nil, err
			}
		}
	}
	store, err := llmwork.NewStore(p.Config, p.Pool)
	if err != nil {
		return nil, err
	}
	store.SetDispatch(p.Dispatch)
	return store, nil
}
