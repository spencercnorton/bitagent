// Package seedsfx wires the tracker-scrape seeds worker into the app. The
// worker refreshes torrent seeders/leechers from authoritative public trackers
// (BEP-15 UDP scrape). Off by default; enable with SEEDS_ENABLED=true and,
// after reviewing dry-run coverage, SEEDS_ENABLE_WRITE=true.
package seedsfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/seeds"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"seeds",
		configfx.NewConfigModule[seeds.Config]("seeds", seeds.NewDefaultConfig()),
		fx.Provide(
			seeds.NewMetrics,
			seeds.New,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *seeds.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
