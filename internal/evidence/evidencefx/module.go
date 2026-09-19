// Package evidencefx wires the evidence ingestor into the app.
//
// Registering this module turns on:
//   - the POST /evidence/arr/:instance webhook endpoint
//   - the qBittorrent poller worker (active only when qb_instances are
//     configured)
//   - the *arr history poller worker (active only when arr_instances
//     are configured)
//   - the liveness resolver (consumes evidence inserts) and the
//     revalidator worker (revives dead infohashes via DHT get_peers)
//   - the outcome-priors resolver (consumes evidence inserts) and the
//     priors expirer worker (resolves stale grabs as failures)
//   - the evidence-specific Prometheus counter/histogram set
//
// Configuration is loaded from the "evidence" config section;
// environment variables use the EVIDENCE_* prefix.
package evidencefx

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/evidence/liveness"
	"github.com/spencercnorton/bitagent/internal/evidence/priors"
	"github.com/spencercnorton/bitagent/internal/evidence/sources/arrpoller"
	"github.com/spencercnorton/bitagent/internal/evidence/sources/arrwebhook"
	"github.com/spencercnorton/bitagent/internal/evidence/sources/qbpoller"
	"github.com/spencercnorton/bitagent/internal/health"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// New returns the fx module. Include in the app module graph to
// activate evidence ingestion.
func New() fx.Option {
	return fx.Module(
		"evidence",
		configfx.NewConfigModule[evidence.Config]("evidence", evidence.NewDefaultConfig()),
		fx.Provide(
			evidence.NewStore,
			evidence.NewMetrics,
			evidence.NewFreshness,
			newFreshnessCheck,
			arrwebhook.New,
			qbpoller.New,
			arrpoller.New,
			// Liveness wiring. The store + metrics are unconditional
			// providers; the resolver and revalidator gate their own
			// behaviour on cfg.Liveness.Enabled, so wiring them here
			// is cheap when disabled.
			liveness.NewStore,
			liveness.NewMetrics,
			newLivenessResolver,
			liveness.NewRevalidator,
			// Priors wiring. Same shape as liveness — store, metrics,
			// resolver, expirer worker. The resolver is gated on
			// cfg.OutcomePriors.Enabled and the expirer's run loop
			// short-circuits when disabled.
			priors.NewStore,
			priors.NewMetrics,
			priors.NewPgSourceLookup,
			newPriorsResolver,
			priors.NewExpirer,
		),
		// Hook the resolvers into the evidence store so every Insert
		// fans out to BOTH the liveness state machine and the priors
		// resolver. The store contract is single-hook; we compose
		// here so each subsystem stays independent.
		fx.Invoke(func(s *evidence.Store, lr *liveness.Resolver, pr *priors.Resolver) {
			s.SetPostInsertHook(func(ctx context.Context, ev evidence.Evidence) {
				lr.HandleEvidence(ctx, ev)
				pr.HandleEvidence(ctx, ev)
			})
		}),
		// Export the evidence Metrics struct's collectors into the
		// shared Prometheus collector group so /metrics exposes them.
		fx.Provide(fx.Annotated{
			Group:  "prometheus_collectors,flatten",
			Target: func(m *evidence.Metrics) []prometheus.Collector { return m.Collectors() },
		}),
		fx.Provide(fx.Annotated{
			Group:  "prometheus_collectors,flatten",
			Target: func(m *liveness.Metrics) []prometheus.Collector { return m.Collectors() },
		}),
		fx.Provide(fx.Annotated{
			Group:  "prometheus_collectors,flatten",
			Target: func(m *priors.Metrics) []prometheus.Collector { return m.Collectors() },
		}),
	)
}

// freshnessCheckResult exports the evidence-source staleness check into
// the shared health checker group, so a dark feed shows up on /status.
type freshnessCheckResult struct {
	fx.Out
	Option health.CheckerOption `group:"health_check_options"`
}

// newFreshnessCheck registers the staleness check. Each instance carries its
// own window, recorded by its poller at Track time from that poller's interval,
// so a slow *arr feed cannot buy a fast qB feed extra grace.
func newFreshnessCheck(f *evidence.Freshness) freshnessCheckResult {
	return freshnessCheckResult{
		Option: health.WithPeriodicCheck(time.Minute, time.Minute,
			evidence.NewFreshnessCheck(f)),
	}
}

// newLivenessResolver builds the liveness resolver. Defined here
// rather than in the liveness package itself because the resolver
// depends on evidence.Config, which is loaded in this module.
func newLivenessResolver(
	store *liveness.Store,
	cfg evidence.Config,
	metrics *liveness.Metrics,
	logger *zap.SugaredLogger,
) *liveness.Resolver {
	return liveness.NewResolver(store, cfg.Liveness, metrics, logger.Named("liveness"))
}

// newPriorsResolver builds the priors resolver. Same rationale as
// newLivenessResolver.
func newPriorsResolver(
	store *priors.Store,
	sources priors.SourceLookup,
	cfg evidence.Config,
	metrics *priors.Metrics,
	logger *zap.SugaredLogger,
) *priors.Resolver {
	return priors.NewResolver(store, sources, cfg.OutcomePriors, metrics, logger.Named("priors"))
}
