package junkpurgefx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
	"github.com/spencercnorton/bitagent/internal/junkpurge/quarantinehttp"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module("quarantine", configfx.NewConfigModule[junkpurge.Config]("junkpurge", junkpurge.NewDefaultConfig()), fx.Provide(junkpurge.NewMetrics,
		fx.Annotate(func(m *junkpurge.Metrics) []prometheus.Collector { return m.Collectors() }, fx.ResultTags(`group:"prometheus_collectors,flatten"`)),
		fx.Annotate(junkpurge.NewMaintenance, fx.ResultTags(`group:"workers"`)), fx.Annotate(quarantinehttp.New, fx.ResultTags(`group:"http_server_options"`))))
}
