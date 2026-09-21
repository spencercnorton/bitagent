package ktable

import (
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable/btree"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Identity lazy.Lazy[protocol.NodeIdentity]
}

type Result struct {
	fx.Out
	Table                lazy.Lazy[Table]
	NodesCountGauge      prometheus.Collector `group:"prometheus_collectors"`
	NodesAddedCounter    prometheus.Collector `group:"prometheus_collectors"`
	NodesDroppedCounter  prometheus.Collector `group:"prometheus_collectors"`
	HashesCountGauge     prometheus.Collector `group:"prometheus_collectors"`
	HashesAddedCounter   prometheus.Collector `group:"prometheus_collectors"`
	HashesDroppedCounter prometheus.Collector `group:"prometheus_collectors"`
}

const (
	nodesK  = 80
	hashesK = 80
)

func New(p Params) Result {
	// Collectors are allocated up front so they register with Prometheus
	// exactly once; the table itself waits for the node identity, which
	// is resolved on first use (external-IP lookup) rather than at
	// construction.
	nodesCollector := newPrometheusCollector("nodes")
	hashesCollector := newPrometheusCollector("hashes")

	table := lazy.New(func() (Table, error) {
		identity, err := p.Identity.Get()
		if err != nil {
			return nil, err
		}
		return newTable(identity.ID, nodesCollector, hashesCollector), nil
	})

	return Result{
		Table:                table,
		NodesCountGauge:      nodesCollector.CountGauge,
		NodesAddedCounter:    nodesCollector.AddedCounter,
		NodesDroppedCounter:  nodesCollector.DroppedCounter,
		HashesCountGauge:     hashesCollector.CountGauge,
		HashesAddedCounter:   hashesCollector.AddedCounter,
		HashesDroppedCounter: hashesCollector.DroppedCounter,
	}
}

func newTable(nodeID ID, nodesCollector, hashesCollector btree.PrometheusCollector) Table {
	rm := &reverseMap{addrs: make(map[string]*infoForAddr)}
	nodes := nodeKeyspace{
		keyspace: newKeyspace[netip.AddrPort, NodeOption, Node, *node](
			nodeID,
			nodesK,
			func(id ID, addr netip.AddrPort) *node {
				return &node{
					nodeBase: nodeBase{
						id:   id,
						addr: addr,
					},
					discoveredAt: time.Now(),
					reverseMap:   rm,
				}
			},
		),
	}
	patchPrometheusCollector(nodesCollector, &nodes.keyspace)
	hashes := hashKeyspace{
		keyspace: newKeyspace[[]HashPeer, HashOption, Hash, *hash](
			nodeID,
			hashesK,
			func(id ID, peers []HashPeer) *hash {
				peersMap := make(map[string]HashPeer, len(peers))
				for _, p := range peers {
					peersMap[p.Addr.Addr().String()] = p
					rm.putAddrHashes(p.Addr.Addr(), id)
				}
				return &hash{
					id:           id,
					peers:        peersMap,
					discoveredAt: time.Now(),
					reverseMap:   rm,
				}
			},
		),
	}
	patchPrometheusCollector(hashesCollector, &hashes.keyspace)

	return &table{
		origin:  nodeID,
		nodesK:  nodesK,
		hashesK: hashesK,
		nodes:   nodes,
		hashes:  hashes,
		addrs:   rm,
	}
}

const (
	namespace = "bitagent"
	subsystem = "dht_ktable"
)

func newPrometheusCollector(itemName string) btree.PrometheusCollector {
	return btree.PrometheusCollector{
		CountGauge: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      itemName + "_count",
			Help:      "Number of " + itemName + " in routing table.",
		}),
		AddedCounter: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      itemName + "_added",
			Help:      "Total number of " + itemName + " added to routing table.",
		}),
		DroppedCounter: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      itemName + "_dropped",
			Help:      "Total number of " + itemName + " dropped from routing table.",
		}),
	}
}

// patchPrometheusCollector wraps the keyspace's btree in the given
// (already registered) collector.
func patchPrometheusCollector[
	Input any,
	Option any,
	ItemPublic keyspaceItem,
	ItemPrivate keyspaceItemPrivate[Input, Option, ItemPublic],
](collector btree.PrometheusCollector, ks *keyspace[Input, Option, ItemPublic, ItemPrivate]) {
	collector.Btree = ks.btree
	ks.btree = collector
}
