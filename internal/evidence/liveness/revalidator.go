package liveness

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/client"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
	"github.com/spencercnorton/bitagent/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// peerLookup is the subset of client.Client the revalidator
// exercises. It is defined locally so tests can stub the DHT
// without dragging in the prometheus-decorated wrapper.
type peerLookup interface {
	GetPeers(ctx context.Context, addr netip.AddrPort, infoHash protocol.ID) (client.GetPeersResult, error)
}

// closestNodes is the subset of ktable.Table the revalidator uses
// to pick query targets. Same rationale as peerLookup.
type closestNodes interface {
	GetClosestNodes(id ktable.ID) []ktable.Node
}

// revalidateStore is the persistence subset the revalidator's
// per-hash logic exercises (rearm + mark-alive happens via the
// resolver, not here).
type revalidateStore interface {
	RearmRevalidate(ctx context.Context, infoHash []byte, ttl time.Duration) error
}

// reviver is the subset of *Resolver the revalidator calls when a
// DHT lookup confirms peers exist.
type reviver interface {
	MarkAliveFromDHT(ctx context.Context, infoHash []byte) error
}

const (
	revalidatorWorkerKey = "evidence_liveness_revalidator"

	// Cap concurrent DHT lookups to avoid hammering the routing
	// table — the crawler's own get_peers traffic is the hot path
	// and we should leave it almost all the bandwidth.
	revalidatorConcurrency = 4

	// blacklistGaugeInterval is how often the worker samples the
	// dead-row count for the gauge. Independent of the revalidate
	// cycle so a long stall_threshold doesn't starve the dashboard.
	blacklistGaugeInterval = 60 * time.Second
)

// RevalidatorParams is the fx dependency tuple. KTable + DHT client
// are optional in tests; in production they are always wired by
// dhtfx.
type RevalidatorParams struct {
	fx.In
	Config   evidence.Config
	Resolver *Resolver
	Store    *Store
	KTable   lazy.Lazy[ktable.Table]
	Client   lazy.Lazy[client.Client]
	Metrics  *Metrics
	Logger   *zap.SugaredLogger
}

// RevalidatorResult exports the worker into the workers group.
type RevalidatorResult struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

// NewRevalidator wires the worker. When liveness is disabled the
// worker is registered but its run loop returns immediately on
// start, so registration is cheap.
func NewRevalidator(p RevalidatorParams) RevalidatorResult {
	r := &revalidator{
		cfg:      p.Config.Liveness,
		resolver: p.Resolver,
		store:    p.Store,
		lazyKT:   p.KTable,
		client:   p.Client,
		metrics:  p.Metrics,
		logger:   p.Logger.Named("liveness-revalidator"),
	}
	return RevalidatorResult{Worker: worker.NewWorker(revalidatorWorkerKey, fx.Hook{
		OnStart: r.start,
		OnStop:  r.stop,
	})}
}

type revalidator struct {
	cfg      evidence.LivenessConfig
	resolver *Resolver
	store    *Store
	lazyKT   lazy.Lazy[ktable.Table]
	kTable   ktable.Table
	client   lazy.Lazy[client.Client]
	metrics  *Metrics
	logger   *zap.SugaredLogger

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (r *revalidator) start(context.Context) error {
	if !r.cfg.Enabled {
		r.logger.Info("liveness disabled; revalidator dormant")
		return nil
	}
	if r.cfg.RevalidateInterval <= 0 {
		return errors.New("liveness: revalidate_interval must be > 0")
	}
	// Resolving the routing table here (not at construction) keeps the
	// external-IP lookup out of processes that never enable this worker.
	kTable, err := r.lazyKT.Get()
	if err != nil {
		return err
	}
	r.kTable = kTable
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.wg.Add(2)
	go r.revalidateLoop(ctx)
	go r.gaugeLoop(ctx)
	return nil
}

func (r *revalidator) stop(context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	return nil
}

// revalidateLoop runs forever pulling the next eligible batch of
// dead infohashes and dispatching DHT lookups against them.
func (r *revalidator) revalidateLoop(ctx context.Context) {
	defer r.wg.Done()

	// Stagger the first run by a short delay so a fleet of
	// containers starting in lockstep doesn't all hit the DHT at
	// the same instant.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	ticker := time.NewTicker(r.cfg.RevalidateInterval)
	defer ticker.Stop()

	r.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runOnce(ctx)
		}
	}
}

// gaugeLoop samples the dead-row count and publishes the gauge.
// Independent of the revalidate cadence.
func (r *revalidator) gaugeLoop(ctx context.Context) {
	defer r.wg.Done()
	ticker := time.NewTicker(blacklistGaugeInterval)
	defer ticker.Stop()
	r.sampleGauge(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sampleGauge(ctx)
		}
	}
}

func (r *revalidator) sampleGauge(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := r.store.Count(cctx, StatusDead)
	if err != nil {
		// Don't spam — the counter is best-effort.
		return
	}
	r.metrics.SetBlacklistSize(float64(n))
}

// runOnce is one revalidation cycle: select up to BatchSize dead
// hashes, fan out DHT lookups bounded by revalidatorConcurrency.
func (r *revalidator) runOnce(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.RevalidateInterval)
	defer cancel()
	hashes, err := r.store.DueForRevalidation(cctx, r.cfg.RevalidateBatchSize)
	if err != nil {
		r.logger.Warnw("revalidator: due-for-revalidation", "err", err)
		r.metrics.Revalidation("error")
		return
	}
	if len(hashes) == 0 {
		return
	}
	r.logger.Infow("revalidator cycle", "candidates", len(hashes))

	sem := make(chan struct{}, revalidatorConcurrency)
	var wg sync.WaitGroup
dispatch:
	for _, h := range hashes {
		// Acquire a slot first; if shutdown fires we abandon the
		// remaining batch entirely. The earlier (broken) version
		// used an unlabeled break which only exited the select,
		// then spawned a goroutine that blocked on the never-acquired
		// semaphore receive — guaranteeing wg.Wait() never returned.
		select {
		case <-ctx.Done():
			break dispatch
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(infoHash []byte) {
			defer wg.Done()
			defer func() { <-sem }()
			r.revalidateOne(ctx, infoHash)
		}(h)
	}
	wg.Wait()
}

// revalidateOne queries get_peers against the closest known nodes
// for the infohash and counts unique peer addresses returned. If
// the count meets or exceeds the configured floor the row is
// flipped back to alive; otherwise next_revalidate_at is rearmed.
func (r *revalidator) revalidateOne(parentCtx context.Context, infoHash []byte) {
	dhtClient, err := r.client.Get()
	if err != nil {
		r.metrics.Revalidation("error")
		return
	}
	revalidateOneWith(parentCtx, infoHash, r.cfg, r.kTable, dhtClient, r.store, r.resolver, r.metrics)
}

// revalidateOneWith is the dependency-free body of revalidateOne,
// extracted so tests can drive it with stubs for the DHT client and
// routing table without spinning up the full fx graph.
func revalidateOneWith(
	parentCtx context.Context,
	infoHash []byte,
	cfg evidence.LivenessConfig,
	table closestNodes,
	dhtClient peerLookup,
	store revalidateStore,
	resolver reviver,
	metrics *Metrics,
) {
	cctx, cancel := context.WithTimeout(parentCtx, cfg.RevalidateTimeout)
	defer cancel()

	id, ok := infoHashToID(infoHash)
	if !ok {
		metrics.Revalidation("error")
		return
	}

	nodes := table.GetClosestNodes(id)
	if len(nodes) == 0 {
		// No closest nodes known — not an error, just no signal.
		// Rearm and move on. We do NOT count this as still_dead
		// because we never asked anyone.
		_ = store.RearmRevalidate(cctx, infoHash, cfg.BlacklistTTL())
		return
	}

	peerSet := make(map[string]struct{})
queryLoop:
	for _, node := range nodes {
		select {
		case <-cctx.Done():
			break queryLoop
		default:
		}
		res, qerr := dhtClient.GetPeers(cctx, node.Addr(), id)
		if qerr != nil {
			continue
		}
		for _, p := range res.Values {
			peerSet[p.String()] = struct{}{}
		}
		if len(peerSet) >= cfg.RevalidateMinPeers {
			break queryLoop
		}
	}

	if len(peerSet) >= cfg.RevalidateMinPeers {
		if err := resolver.MarkAliveFromDHT(cctx, infoHash); err != nil {
			metrics.Revalidation("error")
			return
		}
		metrics.Revalidation("alive_again")
		return
	}
	if err := store.RearmRevalidate(cctx, infoHash, cfg.BlacklistTTL()); err != nil {
		metrics.Revalidation("error")
		return
	}
	metrics.Revalidation("still_dead")
}

// infoHashToID converts a bytea info_hash from the store back into
// a protocol.ID for the DHT call. Returns ok=false on length
// mismatch.
func infoHashToID(b []byte) (protocol.ID, bool) {
	var id protocol.ID
	if len(b) != len(id) {
		return id, false
	}
	copy(id[:], b)
	return id, true
}
