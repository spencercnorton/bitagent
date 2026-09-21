package dhtfx

import (
	"context"
	"errors"
	"net/netip"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/config/configfx"
	"github.com/spencercnorton/bitagent/internal/lazy"
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
			provideNodeIdentity,
			client.New,
			ktable.New,
			responder.New,
			server.New,
		),
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

type identityParams struct {
	fx.In

	Config     externalip.FxConfig
	Resolver   externalip.Resolver
	Metrics    *externalip.WatcherMetrics
	Shutdowner fx.Shutdowner
	Logger     *zap.SugaredLogger
}

type identityResult struct {
	fx.Out

	Identity lazy.Lazy[protocol.NodeIdentity]
	AppHook  fx.Hook `group:"app_hooks"`
}

// provideNodeIdentity wires the BEP-42 pipeline behind a lazy so the
// external-IP lookup happens on the first DHT consumer (`ktable`,
// `client`) rather than at fx construction. A process whose enabled
// workers never touch the DHT (e.g. `worker run --keys ui`) makes no
// outbound lookup, logs no node-id line and never runs the watcher.
//
// The watcher starts the moment the identity is resolved — same as the
// dht server, which starts its socket inside its lazy and stops it via
// `app_hooks` — so under `--all` the tuple it observes is unchanged.
func provideNodeIdentity(p identityParams) identityResult {
	log := p.Logger.Named("externalip.watcher")

	// ctx is created up front so an OnStop can precede first Get()
	// without leaking a watcher goroutine (see internal/ui/worker.go).
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

	identity := lazy.New(func() (protocol.NodeIdentity, error) {
		id := resolveNodeIdentity(p.Config, p.Resolver, p.Metrics, p.Logger)
		watcher := externalip.NewWatcher(
			p.Resolver,
			id.ID,
			id.ExternalIP,
			id.RandomFallback,
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
		go watcher.Run(ctx)
		return id, nil
	})

	return identityResult{
		Identity: identity,
		AppHook: fx.Hook{OnStop: func(context.Context) error {
			cancel()
			return nil
		}},
	}
}

// resolveNodeIdentity resolves the crawler's DHT node ID.
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
//
// The watcher that follows is intentionally unchanged in behaviour:
//
//  1. No in-flight ID swap. Kademlia caches our ID at peers; a clean
//     restart is less disruptive than trying to hot-rotate.
//  2. No restart on resolver error. Only confirmed, validated,
//     BEP-42-invalidating IPs fire.
//  3. No restart on raw IP inequality. The trigger condition is
//     `!VerifySecureNodeID(currentID, newIP)`, which is what peers
//     actually enforce.
func resolveNodeIdentity(
	cfg externalip.FxConfig,
	resolver externalip.Resolver,
	metrics *externalip.WatcherMetrics,
	logger *zap.SugaredLogger,
) protocol.NodeIdentity {
	log := logger.Named("dht_node_id")

	// Startup path gets its own (short) deadline — we'd rather boot
	// in random-fallback mode and let the watcher recover than hold
	// the first DHT worker on a slow IP echo service.
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

		return protocol.NodeIdentity{
			ID:             protocol.RandomNodeIDWithClientSuffix(),
			RandomFallback: true,
		}
	}

	id, err := protocol.SecureNodeID(ip)
	if err != nil {
		log.Warnw("BEP-42 node ID derivation failed; falling back to random node ID",
			"external_ip", ip.String(), "err", err.Error())

		metrics.SetSecureNodeIDValid(false)

		return protocol.NodeIdentity{
			ID:             protocol.RandomNodeIDWithClientSuffix(),
			RandomFallback: true,
		}
	}

	log.Infow("DHT node ID derived from external IP (BEP-42 compliant)",
		"external_ip", ip.String())
	metrics.SetSecureNodeIDValid(true)

	return protocol.NodeIdentity{
		ID:         id,
		ExternalIP: ip,
	}
}
