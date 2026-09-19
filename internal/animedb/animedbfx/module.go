// Package animedbfx wires the deterministic anime-titles backbone into the app:
// the persisted alias table, its scheduled refresh worker, and the in-memory
// Resolver the classifier consults. The refresh worker is off by default;
// enable with ANIME_TITLES_ENABLED=true and, after reviewing dry-run build
// counts, ANIME_TITLES_ENABLE_WRITE=true. The Resolver serves the baked seed
// set plus the last persisted table regardless of the worker's state.
package animedbfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/animedb"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"animedb",
		configfx.NewConfigModule[animedb.Config]("anime_titles", animedb.NewDefaultConfig()),
		fx.Provide(
			animedb.NewMetrics,
			animedb.NewStore,
			animedb.NewResolver,
			animedb.NewRunner,
			animedb.New,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *animedb.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
