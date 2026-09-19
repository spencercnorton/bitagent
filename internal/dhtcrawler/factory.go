package dhtcrawler

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/concurrency"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/client"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/banning"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/metainforequester"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"github.com/spencercnorton/bitagent/internal/wantbridge"
	"github.com/spencercnorton/bitagent/internal/worker"
	boom "github.com/tylertreat/BoomFilters"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Config Config
	// ContentFilter + ContentFilterMetrics are now provided by
	// contentfilterfx so the SAME *contentfilter.Filter (and its
	// LLM cache + daily budget + rule miner) is shared with the
	// post-classifier hook in internal/processor. Previously the
	// crawler factory constructed both locally; now it just
	// consumes them.
	ContentFilter        *contentfilter.Filter
	ContentFilterMetrics *contentfilter.Metrics
	KTable               ktable.Table
	Client               lazy.Lazy[client.Client]
	MetainfoRequester    metainforequester.Requester
	BanningChecker       banning.Checker `name:"metainfo_banning_checker"`
	Wantbridge           wantbridge.Wantbridge
	CsamBlocklist        csamblocklist.Manager
	VerdictsStore        *verdicts.Store
	VerdictsConfig       verdicts.Config
	VerdictsMetrics      *verdicts.Metrics
	Search               lazy.Lazy[search.Search]
	Dao                  lazy.Lazy[*dao.Query]
	BlockingManager      lazy.Lazy[blocking.Manager]
	DiscoveredNodes      concurrency.BatchingChannel[ktable.Node] `name:"dht_discovered_nodes"`
	// RandomFallback is true iff the node ID pipeline booted in
	// random-fallback mode (external IP could not be resolved →
	// node ID is not BEP-42 derived). Emitted by dhtfx; see the external-IP watcher.
	// The crawler uses this to gate outbound sample_infohashes,
	// which is the RPC peers actively refuse from non-compliant
	// IDs — running it while non-compliant is wasted bandwidth.
	RandomFallback bool `name:"dht_random_fallback"`
	Logger         *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Worker worker.Worker `group:"workers"`

	DhtCrawlerActive *concurrency.AtomicValue[bool] `name:"dht_crawler_active"`

	PersistedTotal prometheus.Collector `group:"prometheus_collectors"`
	// ContentFilterMetrics removed: contentfilterfx now flattens its
	// collectors into the prometheus_collectors group directly.
}

func New(params Params) Result {
	active := &concurrency.AtomicValue[bool]{}

	var c crawler

	persistedTotal := dualemit.NewCounterVec(prometheus.CounterOpts{
		Namespace: "bitagent",
		Subsystem: "dht_crawler",
		Name:      "persisted_total",
		Help:      "A counter of persisted database entities.",
	}, []string{"entity"})

	// Content filter (shared instance, provided by contentfilterfx).
	// The PRE-classifier deterministic pass fires from the BEP-9
	// success hook below; the POST-classifier LLM-aware pass fires
	// from internal/processor after the CEL classifier emits a
	// language tag. Both paths use this same Filter so the LLM
	// cache, daily budget, and rule miner are SHARED — that's why
	// the construction was lifted out of this factory and exposed
	// as an fx provider.
	cfMetrics := params.ContentFilterMetrics
	cf := params.ContentFilter

	// Bootstrap state persistence. When configured, we load the prior
	// snapshot at start and merge those addresses into the compiled-
	// in bootstrap list so the crawler can re-join the mesh faster
	// than a cold start would. A nil / disabled store is a no-op.
	bootstrapPersist := newBootstrapStore(
		params.Config.BootstrapStatePath,
		int(params.Config.BootstrapStateSize),
	)

	return Result{
		Worker: worker.NewWorker(
			"dht_crawler",
			fx.Hook{
				OnStart: func(context.Context) error {
					active.Set(true)
					scalingFactor := int(params.Config.ScalingFactor)
					cl, err := params.Client.Get()
					if err != nil {
						return err
					}
					query, err := params.Dao.Get()
					if err != nil {
						return err
					}
					blockingManager, err := params.BlockingManager.Get()
					if err != nil {
						return err
					}

					// Merge persisted addrs in FRONT of the compiled-in
					// bootstrap list. On a fresh container with no prior
					// snapshot this is a no-op; on a restart after a
					// BEP-42 IP change we avoid the RTT against
					// the well-known routers.
					bootstrapNodes := params.Config.BootstrapNodes
					if bootstrapPersist.enabled() {
						persisted, loadErr := bootstrapPersist.Load()
						if loadErr != nil {
							params.Logger.Named("dht_crawler").Warnw(
								"bootstrap state load failed; using compiled-in list only",
								"path", params.Config.BootstrapStatePath,
								"err", loadErr.Error())
						} else if len(persisted) > 0 {
							params.Logger.Named("dht_crawler").Infow(
								"loaded persisted bootstrap addrs; prepending to compiled-in list",
								"count", len(persisted),
								"path", params.Config.BootstrapStatePath)

							merged := make([]string, 0, len(persisted)+len(bootstrapNodes))
							for _, a := range persisted {
								merged = append(merged, a.String())
							}
							merged = append(merged, bootstrapNodes...)
							bootstrapNodes = merged
						}
					}

					c = crawler{
						kTable:                       params.KTable,
						client:                       cl,
						metainfoRequester:            params.MetainfoRequester,
						banningChecker:               params.BanningChecker,
						bootstrapNodes:               bootstrapNodes,
						reseedBootstrapNodesInterval: time.Minute * 10,
						getOldestNodesInterval:       time.Second * 10,
						oldPeerThreshold:             time.Minute * 15,
						discoveredNodes:              params.DiscoveredNodes,
						nodesForPing: concurrency.NewBufferedConcurrentChannel[ktable.Node](
							scalingFactor, scalingFactor),
						nodesForFindNode: concurrency.NewBufferedConcurrentChannel[ktable.Node](
							10*scalingFactor, 10*scalingFactor),
						nodesForSampleInfoHashes: concurrency.NewBufferedConcurrentChannel[ktable.Node](
							10*scalingFactor,
							10*scalingFactor,
						),
						infoHashTriage: concurrency.NewBatchingChannel[nodeHasPeersForHash](
							10*scalingFactor, 1000, 20*time.Second),
						getPeers: concurrency.NewBufferedConcurrentChannel[nodeHasPeersForHash](
							10*scalingFactor, 20*scalingFactor),
						scrape: concurrency.NewBufferedConcurrentChannel[nodeHasPeersForHash](
							10*scalingFactor, 20*scalingFactor),
						requestMetaInfo: concurrency.NewBufferedConcurrentChannel[infoHashWithPeers](
							10*scalingFactor,
							metainfoWorkers(params.Config, scalingFactor),
						),
						persistTorrents: concurrency.NewBatchingChannel[infoHashWithMetaInfo](
							1000,
							1000,
							time.Minute,
						),
						persistSources: concurrency.NewBatchingChannel[infoHashWithScrape](
							1000,
							1000,
							time.Minute,
						),
						saveFilesThreshold: params.Config.SaveFilesThreshold,
						savePieces:         params.Config.SavePieces,
						rescrapeThreshold:  params.Config.RescrapeThreshold,
						dao:                query,
						ignoreHashes: &ignoreHashes{
							bloom: boom.NewStableBloomFilter(10_000_000, 2, 0.001),
						},
						blockingManager:      blockingManager,
						soughtNodeID:         &concurrency.AtomicValue[protocol.ID]{},
						randomFallback:       params.RandomFallback,
						stopped:              make(chan struct{}),
						persistedTotal:       persistedTotal,
						contentFilter:        cf,
						contentFilterMetrics: cfMetrics,
						wantbridge:           params.Wantbridge,
						csamBlocklist:        params.CsamBlocklist,
						verdicts:             params.VerdictsStore,
						verdictsLive:         params.VerdictsConfig.ReadersEnabled,
						verdictsMetrics:      params.VerdictsMetrics,
						logger:               params.Logger.Named("dht_crawler"),
					}
					c.soughtNodeID.Set(protocol.RandomNodeID())

					// todo: Fix!
					//nolint:contextcheck
					go c.start()
					return nil
				},
				OnStop: func(context.Context) error {
					active.Set(false)
					if c.stopped != nil {
						close(c.stopped)
					}

					// Persist a snapshot of the current ktable
					// membership. Best-effort: a failure to write is
					// logged but doesn't fail shutdown. The `if
					// c.kTable != nil` guard covers the rare
					// OnStop-without-OnStart ordering (e.g. earlier
					// fx hook errored so the crawler never got built).
					if bootstrapPersist.enabled() && c.kTable != nil {
						addrs := c.kTable.SnapshotNodeAddrs(int(params.Config.BootstrapStateSize))
						if saveErr := bootstrapPersist.Save(addrs); saveErr != nil {
							params.Logger.Named("dht_crawler").Warnw(
								"bootstrap state save failed",
								"path", params.Config.BootstrapStatePath,
								"count", len(addrs),
								"err", saveErr.Error())
						} else {
							params.Logger.Named("dht_crawler").Infow(
								"bootstrap state saved",
								"count", len(addrs),
								"path", params.Config.BootstrapStatePath)
						}
					}
					return nil
				},
			},
		),
		PersistedTotal:   persistedTotal,
		DhtCrawlerActive: active,
	}
}

// metainfoWorkers returns the per-process cap on concurrent BEP-9
// metadata fetches. Honors Config.MetainfoConcurrency when set
// (operator override); otherwise falls back to the legacy
// 40 × ScalingFactor formula. Centralised so the factory + tests
// agree on the resolution rule.
func metainfoWorkers(cfg Config, scalingFactor int) int {
	if cfg.MetainfoConcurrency > 0 {
		return int(cfg.MetainfoConcurrency)
	}
	return 40 * scalingFactor
}

// buildContentFilter was moved to internal/classifier/contentfilter/
// contentfilterfx so the same Filter instance can be shared across
// the dhtcrawler and the post-classifier hook in internal/processor.
