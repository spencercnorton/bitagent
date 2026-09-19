// Package wantbridgefx wires the wantbridge service into the app.
//
// Conservative by default: WANTBRIDGE_ENABLED=false makes everything
// here a pure no-op. Including this module in the app graph is safe
// even before any *arr is configured — the factory returns a NoOp
// Wantbridge that costs nothing at runtime.
//
// What it provides:
//
//   - configfx section "wantbridge" (env prefix WANTBRIDGE_*)
//   - wantbridge.Wantbridge interface (consumed by the dhtcrawler
//     queue-priority shim in a follow-up MR; nothing consumes it
//     yet at this MR)
//   - *wantbridge.Metrics
//   - prometheus_collectors fx-group entries for the metrics
//   - a worker entry that polls the configured *arr sources
package wantbridgefx

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/wantbridge"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// New returns the fx Option that wires wantbridge into the app.
func New() fx.Option {
	return fx.Module(
		"wantbridge",
		configfx.NewConfigModule[wantbridge.Config]("wantbridge", wantbridge.NewDefaultConfig()),
		fx.Provide(
			wantbridge.NewMetrics,
			provideWantbridge,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *wantbridge.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
		fx.Provide(fx.Annotated{
			Group:  "workers",
			Target: provideWantbridgeWorker,
		}),
	)
}

// provideWantbridge is the fx-injected factory for the wantbridge.
// Dependencies: Config (from configfx), Metrics (just provided),
// Logger.
//
// Returns the interface, not the concrete *Service, so consumers
// see a stable contract. The factory selects NoOp vs Service per
// the operator's config.
func provideWantbridge(
	cfg wantbridge.Config,
	metrics *wantbridge.Metrics,
	logger *zap.SugaredLogger,
) wantbridge.Wantbridge {
	return wantbridge.NewWith(
		cfg,
		logger.Named("wantbridge"),
		metrics.Callbacks(),
	)
}

// provideWantbridgeWorker registers the periodic poll loop with the
// app's worker manager. When wantbridge is disabled / no sources
// configured, the worker is a no-op (Service.Run short-circuits on
// !Enabled, and NoOp's Run via type assertion is also a no-op).
//
// Why a worker entry rather than fx.Lifecycle: the codebase's
// standard for periodic background loops is the worker abstraction,
// which surfaces in healthchecks + the dashboard. Same pattern as
// retentionfx + dhtcrawlerfx.
//
// Lifecycle: OnStart creates a cancellable context derived from
// context.Background (fx start hooks must return promptly, can't
// reuse the start ctx for a long-lived loop), passes it to
// Service.Run, and OnStop cancels it. Service.Run watches ctx.Done
// in its inner ticker loop and exits cleanly. Without this we'd
// leak the poll goroutine on app shutdown — Jeeves caught this on
func provideWantbridgeWorker(wb wantbridge.Wantbridge, logger *zap.SugaredLogger) worker.Worker {
	var (
		runCtx    context.Context
		runCancel context.CancelFunc
	)
	return worker.NewWorker(
		"wantbridge",
		fx.Hook{
			OnStart: func(_ context.Context) error {
				runCtx, runCancel = context.WithCancel(context.Background())
				// Honesty: wantbridge is wired but OBSERVE-ONLY. The
				// dhtcrawler calls Match() (request_meta_info.go) and
				// discards the result — it feeds bitagent_wantbridge_*
				// metrics but does NOT yet re-prioritise the BEP-9
				// fetch queue, skip Tier-2 discoveries, or change
				// Torznab results (the priority-queue / persist-
				// annotation wiring is deferred — see doc.go D4/D5).
				// Warn the operator once at startup so enabling this
				// isn't mistaken for a working fetch-prioritisation
				// feature. Only the live Service reports Enabled()==true;
				// the NoOp (disabled / no *arr sources) stays quiet.
				if wb.Enabled() {
					logger.Warnw(
						"wantbridge: observe-only mode — Match() emits bitagent_wantbridge_* metrics " +
							"but BEP-9 fetch prioritization is NOT yet wired (deferred); enabling this " +
							"does not change fetch order or Torznab results",
					)
				}
				// Only the live Service has a Run method; NoOp
				// doesn't need one. Type-assert and start the
				// poll loop for the live path.
				if runner, ok := wb.(interface{ Run(context.Context) }); ok {
					runner.Run(runCtx)
				}
				return nil
			},
			OnStop: func(_ context.Context) error {
				if runCancel != nil {
					runCancel()
				}
				return nil
			},
		},
	)
}
