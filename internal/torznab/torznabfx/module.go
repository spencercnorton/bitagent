package torznabfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/evidence/liveness"
	"github.com/spencercnorton/bitagent/internal/evidence/priors"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"github.com/spencercnorton/bitagent/internal/torznab/adapter"
	"github.com/spencercnorton/bitagent/internal/torznab/httpserver"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"go.uber.org/fx"
)

// torznabClientDeps groups the inputs to the lazy adapter factory
// so the fx.Provide signature stays compact.
type torznabClientDeps struct {
	fx.In
	Search          lazy.Lazy[search.Search]
	TorznabConfig   torznab.Config
	EvidenceConfig  evidence.Config
	LivenessStore   *liveness.Store
	LivenessMetrics *liveness.Metrics
	PriorsStore     *priors.Store
	PriorsMetrics   *priors.Metrics
	VerdictsStore   *verdicts.Store
	VerdictsConfig  verdicts.Config
	VerdictsMetrics *verdicts.Metrics
}

func New() fx.Option {
	return fx.Module(
		"torznab",
		configfx.NewConfigModule[torznab.Config]("torznab", torznab.NewDefaultConfig()),
		fx.Provide(
			func(deps torznabClientDeps) lazy.Lazy[torznab.Client] {
				return lazy.New[torznab.Client](func() (torznab.Client, error) {
					s, err := deps.Search.Get()
					if err != nil {
						return nil, err
					}
					var ranker adapter.Reranker
					if deps.EvidenceConfig.OutcomePriors.Enabled {
						// The Ranker is constructed lazily here so
						// the priors module is only consulted when
						// the operator has explicitly enabled it.
						// Apply=false leaves ranking inert (shadow
						// mode) — see priors/doc.go for the rollout.
						ranker = priors.NewRanker(
							deps.PriorsStore,
							deps.EvidenceConfig.OutcomePriors,
							deps.PriorsMetrics,
						)
					}
					return adapter.NewWithFiltersAndRanker(
						s,
						deps.LivenessStore,
						deps.EvidenceConfig.Liveness.Enabled,
						deps.LivenessMetrics,
						adapter.FreshnessConfigFromTorznab(deps.TorznabConfig),
						ranker,
					).WithVerdicts(
						deps.VerdictsStore,
						deps.VerdictsConfig.ReadersEnabled,
						deps.VerdictsMetrics,
					), nil
				})
			},
			httpserver.NewMetrics,
			fx.Annotate(
				httpserver.New,
				fx.ResultTags(`group:"http_server_options"`),
			),
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *httpserver.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
		fx.Decorate(
			func(cfg torznab.Config) torznab.Config {
				return cfg.MergeDefaults()
			}),
	)
}
