package dhtfx

import (
	"context"
	"errors"
	"net/netip"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/client"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/responder"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/server"
	"github.com/spencercnorton/bitagent/internal/protocol/externalip"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func New() fx.Option {
	return fx.Module(
		"dht",
		configfx.NewConfigModule[server.Config]("dht_server", server.NewDefaultConfig()),
		configfx.NewConfigModule[externalip.FxConfig]("dht_externalip", externalip.NewDefaultFxConfig()),
		fx.Provide(
			provideExternalIPResolver,
			provideExternalIPMetrics,
			provideNodeID,
			client.New,
			ktable.New,
			responder.New,
			server.New,
		),
		fx.Invoke(startExternalIPWatcher),
	)
}

func provideExternalIPResolver(
	cfg externalip.FxConfig,
	logger *zap.SugaredLogger,
) (externalip.Resolver, error) {
	return externalip.New(
		externalip.Config{
			Override: cfg.Override,
			Timeout:  cfg.PerCheckTimeout,
		},
		logger.Named("externalip"),
	)
}

// provideExternalIPMetrics returns the watcher metrics AND feeds
// each of its collectors into the shared `prometheus_collectors` fx
// group so telemetry picks them up automatically.
type externalIPMetricsResult struct {
	fx.Out

	Metrics    *externalip.WatcherMetrics
	Collectors []prometheus.Collector `group:"prometheus_collectors,flatten"`
}

func provideExternalIPMetrics() externalIPMetricsResult {
	m := externalip.NewWatcherMetrics()

	return externalIPMetricsResult{
		Metrics:    m,
		Collectors: m.Collectors(),
	}
}

// nodeIDResult is the startup output of the BEP-42 pipeline: the
// node ID the crawler should use, the IP it was derived from (zero
// when we fell back to random), and whether the fallback is active.
// The watcher consumes all three.
type nodeIDResult struct {
	fx.Out

	NodeID         protocol.ID `name:"dht_node_id"`
	InitialIP      netip.Addr  `name:"dht_initial_external_ip"`
	RandomFallback bool        `name:"dht_random_fallback"`
}

// provideNodeID resolves the crawler's DHT node ID.
//
// BEP-42 ("DHT Security extension") ties node ID to external IP via
// CRC32C. Compliant derivation unlocks storage-eligibility on
// enforcing peers — that's the load-bearing inbound win.
//
// Three outcomes:
//
//  1. Resolver returns a validated IP → derive a BEP-42-compliant ID.
//     Log INFO, set `secure_node_id_valid=1`.
//  2. Resolver fails (transport, all sources unreachable, or every
//     source returned a non-public/non-v4 address) → log WARN, fall
//     back to random-with-client-suffix, set `secure_node_id_valid=0`.
//     The watcher polls on; its first confirmed valid resolve will
//     trigger a restart so we graduate into compliance.
//  3. Derivation fails on the resolved IP (pathological) → WARN +
//     fallback as (2).
func provideNodeID(
	cfg externalip.FxConfig,
	resolver externalip.Resolver,
	metrics *externalip.WatcherMetrics,
	logger *zap.SugaredLogger,
) nodeIDResult {
	log := logger.Named("dht_node_id")

	// Startup path gets its own (short) deadline — we'd rather boot
	// in random-fallback mode and let the watcher recover than hold
	// the whole fx graph on a slow IP echo service.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.StartupTimeout)
	defer cancel()

	ip, err := resolver.Resolve(ctx)
	if err != nil {
		level := "transport failure"
		if errors.Is(err, externalip.ErrInvalidAddress) {
			level = "invalid address from all sources"
		}
		log.Warnw(
			"external IP resolution failed at startup; falling back to random node ID "+
				"(BEP-42 non-compliant; enforcing peers will refuse storage queries). "+
				"Watcher will retry and trigger restart once egress IP is resolvable.",
			"reason", level,
			"err", err.Error(),
		)

		metrics.SetSecureNodeIDValid(false)

		return nodeIDResult{
			NodeID:         protocol.RandomNodeIDWithClientSuffix(),
			InitialIP:      netip.Addr{},
			RandomFallback: true,
		}
	}

	id, err := protocol.SecureNodeID(ip)
	if err != nil {
		log.Warnw("BEP-42 node ID derivation failed; falling back to random node ID",
			"external_ip", ip.String(), "err", err.Error())

		metrics.SetSecureNodeIDValid(false)

		return nodeIDResult{
			NodeID:         protocol.RandomNodeIDWithClientSuffix(),
			InitialIP:      netip.Addr{},
			RandomFallback: true,
		}
	}

	log.Infow("DHT node ID derived from external IP (BEP-42 compliant)",
		"external_ip", ip.String())
	metrics.SetSecureNodeIDValid(true)

	return nodeIDResult{
		NodeID:         id,
		InitialIP:      ip,
		RandomFallback: false,
	}
}

type watcherParams struct {
	fx.In

	Config         externalip.FxConfig
	Resolver       externalip.Resolver
	Metrics        *externalip.WatcherMetrics
	NodeID         protocol.ID `name:"dht_node_id"`
	InitialIP      netip.Addr  `name:"dht_initial_external_ip"`
	RandomFallback bool        `name:"dht_random_fallback"`
	Lifecycle      fx.Lifecycle
	Shutdowner     fx.Shutdowner
	Logger         *zap.SugaredLogger
}

// startExternalIPWatcher hooks a background goroutine into the fx
// lifecycle that polls the external IP and fires when the current
// node ID no longer verifies. On confirmed invalidation, it calls
// `fx.Shutdowner.Shutdown()` so the container runtime's restart
// policy brings us back with a fresh node ID.
//
// Intentional non-goals:
//
//  1. No in-flight ID swap. Kademlia caches our ID at peers; a clean
//     restart is less disruptive than trying to hot-rotate.
//  2. No restart on resolver error. Only confirmed, validated,
//     BEP-42-invalidating IPs fire.
//  3. No restart on raw IP inequality. The trigger condition is
//     `!VerifySecureNodeID(currentID, newIP)`, which is what peers
//     actually enforce.
func startExternalIPWatcher(p watcherParams) {
	log := p.Logger.Named("externalip.watcher")

	ctx, cancel := context.WithCancel(context.Background())

	onChange := func(old, current netip.Addr) {
		log.Warnw(
			"external IP invalidates current node ID — shutting down so the container "+
				"restarts with a fresh BEP-42-compliant ID",
			"old", old.String(),
			"current", current.String(),
		)
		if err := p.Shutdowner.Shutdown(); err != nil {
			log.Errorw("shutdowner.Shutdown failed", "err", err.Error())
		}
	}

	watcher := externalip.NewWatcher(
		p.Resolver,
		p.NodeID,
		p.InitialIP,
		p.RandomFallback,
		externalip.WatcherConfig{
			Interval:                 p.Config.Interval,
			JitterFraction:           p.Config.JitterFraction,
			PerCheckTimeout:          p.Config.PerCheckTimeout,
			ConsecutiveConfirmations: p.Config.ConsecutiveConfirmations,
		},
		onChange,
		log,
		p.Metrics,
	)

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go watcher.Run(ctx)
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			return nil
		},
	})
}
