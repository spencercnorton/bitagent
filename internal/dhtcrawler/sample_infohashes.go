package dhtcrawler

import (
	"context"
	"fmt"
	"time"

	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
)

// nextSampleInterval is the backoff this crawler will wait before
// re-querying a given peer's BEP-51 endpoint.
//
// BEP-51 (§"interval") allows a responder to advertise any value in
// [0, 21600] seconds and requires clients to respect it. The original
// upstream bitmagnet behaviour clamped productive peers to 60 s
// regardless of their advertised interval — that is spec-hostile and
// invites rate-limiting / blacklisting from well-behaved peers.
//
// Policy: **honour the peer's advertised interval unchanged.** If the
// crawler needs more throughput, the right answer is to query more
// peers, diversify targets, or improve peer selection — not to
// violate the interval a peer asked for.
//
// `discoveredNew` is retained in the signature (rather than inlining
// `fromPeer` at every call site) so future policy work can reason
// about "did this peer give us anything useful" without reshaping
// the caller again.
func nextSampleInterval(fromPeer, discoveredNew int) int {
	_ = discoveredNew

	return fromPeer
}

func (c *crawler) getNodesForSampleInfoHashes(ctx context.Context) {
	// BEP-42 enforcers reject sample_infohashes from nodes whose ID
	// doesn't derive from their external IP. When we booted in
	// random-fallback mode (external IP unresolvable → non-compliant
	// node ID), every outbound sample_infohashes query is bandwidth
	// we spend on a response that won't come — so we gate the
	// feeder off until either the container is restarted by the
	// external-IP watcher with a fresh compliant ID, or
	// this process is stopped.
	//
	// Inbound sample_infohashes responses from peers still work
	// (served by the responder on our server socket); we're only
	// quieting our outbound side. Other RPCs (ping / find_node /
	// get_peers / scrape) stay active because peers don't enforce
	// BEP-42 on those.
	if c.randomFallback {
		c.logger.Warn(
			"outbound sample_infohashes gated: crawler booted with random-fallback " +
				"node ID (BEP-42 non-compliant). The external-IP watcher will " +
				"restart the container once a valid egress IP is observable; " +
				"inbound queries and all other RPCs remain active.",
		)
		<-ctx.Done()

		return
	}

	for {
		peers := c.kTable.GetNodesForSampleInfoHashes(60)
		for _, p := range peers {
			select {
			case <-ctx.Done():
				return
			case c.nodesForSampleInfoHashes.In() <- p:
				continue
			}
		}

		<-time.After(time.Second)
	}
}

func (c *crawler) runSampleInfoHashes(ctx context.Context) {
	_ = c.nodesForSampleInfoHashes.Run(ctx, func(n ktable.Node) {
		if !n.IsSampleInfoHashesCandidate() {
			return
		}

		res, err := c.client.SampleInfoHashes(ctx, n.Addr(), c.soughtNodeID.Get())
		if err != nil {
			c.kTable.BatchCommand(
				ktable.DropNode{ID: n.ID(), Reason: fmt.Errorf("sample_infohashes failed: %w", err)},
			)

			return
		}

		var discoveredHashes []nodeHasPeersForHash

		for _, s := range res.Samples {
			if !c.ignoreHashes.testAndAdd(s) {
				discoveredHashes = append(discoveredHashes, nodeHasPeersForHash{
					infoHash: s,
					node:     n.Addr(),
				})
			}
		}

		for _, h := range discoveredHashes {
			select {
			case <-ctx.Done():
				return
			case c.infoHashTriage.In() <- h:
				continue
			}
		}

		interval := nextSampleInterval(res.Interval, len(discoveredHashes))

		c.admitNodeFromReply(
			n.ID(),
			n.Addr(),
			res.ReadOnly,
			ktable.NodeBep51Support(true),
			ktable.NodeSampleInfoHashesRes(
				len(discoveredHashes),
				res.Num,
				time.Now().Add(time.Duration(interval)*time.Second),
			),
		)

		if len(res.Nodes) > 0 {
			// block on the channel for up to a second trying to add sampled nodes to the discoveredNodes
			// channel
			go func() {
				timeoutCtx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()

				for _, n := range res.Nodes {
					select {
					case <-timeoutCtx.Done():
						return
					case c.discoveredNodes.In() <- ktable.NewNode(n.ID, n.Addr):
						continue
					}
				}
			}()
		}
	})
}
