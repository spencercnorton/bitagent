// Package verdictsfx wires the T3 verdict ledger: the Store (writers
// dual-write from junkpurge/operator since phase A; readers consult
// BlockedSet since phase B), the reader Config, and the reader Metrics.
//
// Conservative by default: VERDICTS_READERS_ENABLED=false keeps the phase-B
// readers in shadow — they consult the ledger and emit
// bitagent_verdicts_reader_total, but change no serving or crawling
// behavior. Flipping the flag is a deliberate operator action that should
// follow a review of the shadow metrics (design §4, phase B gate).
package verdictsfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"verdicts",
		configfx.NewConfigModule[verdicts.Config]("verdicts", verdicts.NewDefaultConfig()),
		fx.Provide(
			verdicts.NewStore,
			verdicts.NewMetrics,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *verdicts.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
