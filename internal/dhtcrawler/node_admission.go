package dhtcrawler

import (
	"errors"
	"net/netip"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
)

// errPeerIsReadOnly is attached to the DropNode reason when a peer's
// reply carries BEP-43 `ro=1`. Centralising the wording here makes
// the metric label / log substring stable for dashboards.
var errPeerIsReadOnly = errors.New("BEP-43: peer reply set ro=1")

// admitNodeFromReply commits the ktable write for a peer that just
// responded to one of our outbound RPCs (ping / find_node / get_peers /
// get_peers+scrape / sample_infohashes).
//
// BEP-43 §2: "Read-only nodes must not be added to the routing table,
// and not returned in replies to find_node requests." The earlier
// discovery-path fix (responder/node_discovery.go) covered inbound
// queries where the sender advertises `ro=1`. This helper covers the
// corresponding outbound path: a peer we queried, whose reply set
// `ro=1`, is also telling us it's read-only — adding it inflates our
// routing table with dead weight.
//
// Behaviour:
//   - readOnly=false → standard admission (PutNode with NodeResponded()
//     and any caller-supplied extra options, e.g. NodeBep51Support)
//   - readOnly=true → DropNode; a prior entry (if any) is evicted
//     and the peer will not be returned in find_node replies
//
// Extra options are appended after the mandatory `NodeResponded()`
// so the call-site ordering is preserved (in particular the sample-
// infohashes path applies `NodeBep51Support` and
// `NodeSampleInfoHashesRes(...)` after the base "responded" marker).
func (c *crawler) admitNodeFromReply(
	id protocol.ID,
	addr netip.AddrPort,
	readOnly bool,
	extraOptions ...ktable.NodeOption,
) {
	if readOnly {
		c.kTable.BatchCommand(ktable.DropNode{
			ID:     id,
			Reason: errPeerIsReadOnly,
		})

		return
	}

	options := make([]ktable.NodeOption, 0, 1+len(extraOptions))
	options = append(options, ktable.NodeResponded())
	options = append(options, extraOptions...)

	c.kTable.BatchCommand(ktable.PutNode{
		ID:      id,
		Addr:    addr,
		Options: options,
	})
}
