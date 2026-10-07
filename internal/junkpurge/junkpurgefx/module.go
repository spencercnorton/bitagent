// Package junkpurgefx wires the junk-purge worker into the app.
//
// Conservative by default: Enabled=false, so including this module admits no
// new work (it may drain already-paid durable Batch output). Even when
// enabled, EnablePurge defaults to
// false — the worker records LLM verdicts into junkpurge_judgments and emits
// bitagent_junkpurge_would_delete_total but deletes nothing. Flipping
// JUNKPURGE_ENABLE_PURGE=true is a separate, deliberate operator action that
// should follow a review of the dry-run drop-list.
package junkpurgefx

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
	"github.com/spencercnorton/bitagent/internal/junkpurge/quarantinehttp"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"junkpurge",
		configfx.NewConfigModule[junkpurge.Config]("junkpurge", junkpurge.NewDefaultConfig()),
		fx.Provide(
			junkpurge.NewMetrics,
			provideJudge,
			junkpurge.New,
		),
		// Quarantine review API on the shared Gin server (list / restore / delete-now).
		fx.Provide(fx.Annotate(
			quarantinehttp.New,
			fx.ResultTags(`group:"http_server_options"`),
		)),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *junkpurge.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}

func provideJudge(
	cfg junkpurge.Config,
	metrics *junkpurge.Metrics,
	pool lazy.Lazy[*pgxpool.Pool],
	audit struct {
		fx.In
		Capture    llmcapture.Capturer `optional:"true"`
		NamePolicy *namepolicy.Policy  `optional:"true"`
	},
) junkpurge.Judge {
	j := junkpurge.NewAuditedJudge(
		cfg, metrics, llmmatch.NewPostgresJunkPurgeCallBudget(pool), audit.Capture,
	)
	if x, ok := j.(interface{ SetNamePolicy(*namepolicy.Policy) }); ok {
		x.SetNamePolicy(audit.NamePolicy)
	}
	return j
}
