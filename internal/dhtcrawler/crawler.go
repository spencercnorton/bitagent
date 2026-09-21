package dhtcrawler

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/bloom"
	"github.com/spencercnorton/bitagent/internal/classifier/contentfilter"
	"github.com/spencercnorton/bitagent/internal/concurrency"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/client"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/banning"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo/metainforequester"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"github.com/spencercnorton/bitagent/internal/wantbridge"
	boom "github.com/tylertreat/BoomFilters"
	"go.uber.org/zap"
)

type crawler struct {
	kTable                       ktable.Table
	client                       client.Client
	metainfoRequester            metainforequester.Requester
	banningChecker               banning.Checker
	bootstrapNodes               []string
	reseedBootstrapNodesInterval time.Duration
	getOldestNodesInterval       time.Duration
	oldPeerThreshold             time.Duration
	discoveredNodes              concurrency.BatchingChannel[ktable.Node]
	nodesForPing                 concurrency.BufferedConcurrentChannel[ktable.Node]
	nodesForFindNode             concurrency.BufferedConcurrentChannel[ktable.Node]
	nodesForSampleInfoHashes     concurrency.BufferedConcurrentChannel[ktable.Node]
	infoHashTriage               concurrency.BatchingChannel[nodeHasPeersForHash]
	getPeers                     concurrency.BufferedConcurrentChannel[nodeHasPeersForHash]
	scrape                       concurrency.BufferedConcurrentChannel[nodeHasPeersForHash]
	requestMetaInfo              concurrency.BufferedConcurrentChannel[infoHashWithPeers]
	persistTorrents              concurrency.BatchingChannel[infoHashWithMetaInfo]
	persistSources               concurrency.BatchingChannel[infoHashWithScrape]
	rescrapeThreshold            time.Duration
	saveFilesThreshold           uint
	savePieces                   bool
	dao                          *dao.Query
	// ignoreHashes is a thread-safe bloom filter that the crawler keeps in memory,
	// containing every hash it has already encountered.
	// This avoids multiple attempts to crawl the same hash, and takes a lot of load off the database query
	// that checks if a hash has already been indexed.
	ignoreHashes    *ignoreHashes
	blockingManager blocking.Manager
	// soughtNodeID is a random node ID used as the target for find_node and sample_infohashes requests.
	// It is rotated every 10 seconds.
	soughtNodeID *concurrency.AtomicValue[protocol.ID]
	// randomFallback is true iff the crawler's node ID is a random
	// fallback (not BEP-42 derived). Enforcing peers reject
	// storage-eligible queries (sample_infohashes) from
	// non-compliant nodes, so while this is true the outbound
	// sample_infohashes worker sits idle to avoid spending
	// bandwidth on queries that will be refused. Set once at boot
	// from the dhtfx node identity (lazy); the external-IP
	// watcher's OnChange triggers a container restart when the
	// crawler graduates to a compliant ID.
	randomFallback bool
	stopped        chan struct{}
	persistedTotal *dualemit.CounterVec
	// contentFilter + contentFilterMetrics enforce the operator's
	// curation rules at metadata-fetch-success time. Either can be
	// nil (Enabled=false) and the request_meta_info.go hook will
	// short-circuit. See internal/classifier/contentfilter for the
	// rule set.
	contentFilter        *contentfilter.Filter
	contentFilterMetrics *contentfilter.Metrics
	// wantbridge surfaces *arr wantlist matches against the just-fetched
	// torrent name. It's wired through the fx graph as the Wantbridge
	// interface — a NoOp implementation when WANTBRIDGE_ENABLED=false,
	// so the field is always non-nil and the call site doesn't gate on
	// it. See internal/wantbridge for the matcher + tier semantics.
	wantbridge wantbridge.Wantbridge
	// csamBlocklist is the pre-fetch defense for community-known
	// CSAM infohashes (issue #494). Always non-nil — wired as the
	// Manager interface, with a NoOp implementation when no feeds
	// are configured. Consulted in infohash_triage (batch Filter
	// before BlockingManager.Filter) and as a defense-in-depth
	// IsBlocked check in request_meta_info just before the BEP-9
	// fetch begins. See internal/csamblocklist for the feed format
	// and the layered defense rationale.
	csamBlocklist csamblocklist.Manager
	// verdicts is the phase-B ledger consult in infohash_triage
	// (docs/design/verdict-ledger.md §4): a batched BlockedSet lookup
	// piggybacking on the triage batch. Shadow-metered while
	// verdictsLive is false (VERDICTS_READERS_ENABLED); when live,
	// blocked hashes are dropped before get_peers/scrape routing —
	// skip-fetch only, kills the infinite BEP-9 refetch loop for
	// quarantined/blacklisted hashes. Errors are advisory (fail-open):
	// the CSAM bloom egress gate is separate and unaffected.
	// Concrete *verdicts.Store rather than a one-implementation
	// interface: an interface here would turn a (hypothetical) typed-nil
	// Store into a non-nil interface value that passes the nil gate and
	// panics inside BlockedSet.
	verdicts        *verdicts.Store
	verdictsLive    bool
	verdictsMetrics *verdicts.Metrics
	logger          *zap.SugaredLogger
}

func (c *crawler) start() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// start the various pipeline workers
	go c.rotateSoughtNodeID(ctx)
	go c.runDiscoveredNodes(ctx)
	go c.runPing(ctx)
	go c.runFindNode(ctx)
	go c.getNodesForFindNode(ctx)
	go c.runSampleInfoHashes(ctx)
	go c.getNodesForSampleInfoHashes(ctx)
	go c.runInfoHashTriage(ctx)
	go c.runGetPeers(ctx)
	go c.runRequestMetaInfo(ctx)
	go c.runScrape(ctx)
	go c.reseedBootstrapNodes(ctx)
	go c.runPersistTorrents(ctx)
	go c.runPersistSources(ctx)
	go c.getOldNodes(ctx)
	<-c.stopped
}

type nodeHasPeersForHash struct {
	infoHash protocol.ID
	node     netip.AddrPort
}

type infoHashWithMetaInfo struct {
	nodeHasPeersForHash
	metaInfo metainfo.Info
}

type infoHashWithPeers struct {
	nodeHasPeersForHash
	peers []netip.AddrPort
}

type infoHashWithScrape struct {
	nodeHasPeersForHash
	bfsd bloom.Filter
	bfpe bloom.Filter
}

type ignoreHashes struct {
	mutex sync.Mutex
	bloom *boom.StableBloomFilter
}

func (i *ignoreHashes) testAndAdd(id protocol.ID) bool {
	i.mutex.Lock()
	defer i.mutex.Unlock()

	return i.bloom.TestAndAdd(id[:])
}

func (c *crawler) rotateSoughtNodeID(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
			c.soughtNodeID.Set(protocol.RandomNodeID())
		}
	}
}
