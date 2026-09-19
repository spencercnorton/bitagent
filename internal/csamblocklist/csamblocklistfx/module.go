// Package csamblocklistfx wires the CSAM blocklist into the app.
//
// What it provides:
//
//   - configfx section "csam_blocklist" (env prefix CSAM_BLOCKLIST_*)
//   - csamblocklist.Manager interface (consumed by the dhtcrawler
//     pre-fetch hook in infohash_triage)
//   - csamblocklist.Exporter interface (consumed by the processor
//     when CEL emits ErrDeleteTorrent)
//   - *csamblocklist.Metrics
//   - prometheus_collectors fx-group entries for the metrics
//   - a worker entry that runs the periodic-refresh poll loop
//
// Conservative defaults (see csamblocklist.NewDefaultConfig):
//
//   - Enabled=true with no FeedUrls configured → Manager is NoOp,
//     no work happens until operator opts into a feed
//   - ExportEnabled=true writing to a local JSONL log
//   - ExportUpstreamURL="" → no outbound POST until operator opts in
package csamblocklistfx

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// initialRefreshBudget is how long the OnStart hook is willing to
// wait for the first feed-refresh to complete before letting the app
// boot anyway. Bounded so a misconfigured / unreachable feed cannot
// permanently block startup. The CSAM blocklist remains a NoOp until
// the first refresh succeeds — but the dhtcrawler also won't have
// accepted any DHT traffic before fx finishes its OnStart phase, so
// the bounded wait closes the "fresh-deploy gap" Jeeves flagged on
const initialRefreshBudget = 30 * time.Second

// New returns the fx Option that wires csamblocklist into the app.
func New() fx.Option {
	return fx.Module(
		"csamblocklist",
		configfx.NewConfigModule[csamblocklist.Config]("csam_blocklist", csamblocklist.NewDefaultConfig()),
		fx.Provide(
			csamblocklist.NewMetrics,
			provideManager,
			provideExporter,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *csamblocklist.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
		fx.Provide(fx.Annotated{
			Group:  "workers",
			Target: provideRefreshWorker,
		}),
		fx.Invoke(registerExporterShutdown),
	)
}

func provideManager(
	cfg csamblocklist.Config,
	metrics *csamblocklist.Metrics,
	logger *zap.SugaredLogger,
) csamblocklist.Manager {
	return csamblocklist.New(cfg, logger.Named("csamblocklist"), metrics)
}

func provideExporter(
	cfg csamblocklist.Config,
	metrics *csamblocklist.Metrics,
	logger *zap.SugaredLogger,
) csamblocklist.Exporter {
	return csamblocklist.NewExporter(cfg, logger.Named("csam-export"), metrics)
}

// provideRefreshWorker wires the periodic Refresh loop into the
// worker manager. NoOp Manager has no Run method; type assertion
// short-circuits cleanly.
// runStartupRefresh runs the synchronous initial refresh that closes
// the "fresh-deploy gap" review flagged. Bounded by
// initialRefreshBudget so a misconfigured / unreachable feed cannot
// permanently block app startup. Errors are logged and swallowed
// (fail-open: better to boot with an empty filter than not boot at all).
//
// Pulled out of provideRefreshWorker so it's directly testable
// without driving the whole fx wiring.
func runStartupRefresh(ctx context.Context, mgr csamblocklist.Manager, log *zap.SugaredLogger, budget time.Duration) {
	if !mgr.Enabled() {
		return
	}
	refreshCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if err := mgr.Refresh(refreshCtx); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			log.Warnw(
				"initial CSAM blocklist refresh timed out; continuing with empty filter",
				"budget", budget.String(),
			)
		case errors.Is(err, context.Canceled):
			// Outer context was cancelled — caller is shutting down.
			// No further logging needed.
			return
		default:
			log.Warnw(
				"initial CSAM blocklist refresh had errors; continuing",
				"err", err,
			)
		}
	}
}

func provideRefreshWorker(mgr csamblocklist.Manager, logger *zap.SugaredLogger) worker.Worker {
	var (
		runCtx    context.Context
		runCancel context.CancelFunc
	)
	log := logger.Named("csamblocklist-worker")
	return worker.NewWorker(
		"csamblocklist",
		fx.Hook{
			OnStart: func(startCtx context.Context) error {
				// Block startup on the first refresh so the dhtcrawler
				// never accepts traffic with an empty bloom filter
				// (review finding: "Initial blocklist refresh does
				// not block crawler startup").
				runStartupRefresh(startCtx, mgr, log, initialRefreshBudget)

				// Periodic-refresh loop runs in the background for
				// the remainder of app lifetime.
				runCtx, runCancel = context.WithCancel(context.Background())
				if runner, ok := mgr.(interface{ Run(context.Context) }); ok {
					go runner.Run(runCtx)
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

// registerExporterShutdown closes the JSONL file handle on app stop.
func registerExporterShutdown(lc fx.Lifecycle, exp csamblocklist.Exporter) {
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			return exp.Close()
		},
	})
}
