package dhtcrawler

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
	ktable_mocks "github.com/spencercnorton/bitagent/internal/protocol/dht/ktable/mocks"
	"github.com/stretchr/testify/mock"
)

// admitNodeFromReply is the outbound-path BEP-43 helper: when a peer
// we queried replies with ro=1, we MUST drop (not admit) them. The
// earlier discovery-path fix handled inbound senders; this helper
// closes the mirror gap on the outbound side. Tests here pin both
// sides of that policy.

// TestAdmitNodeFromReply_readOnlyDrops — peer replied ro=1 → DropNode.
func TestAdmitNodeFromReply_readOnlyDrops(t *testing.T) {
	t.Parallel()

	table := ktable_mocks.NewTable(t)
	c := &crawler{kTable: table}

	id := protocol.RandomNodeID()
	addr := netip.MustParseAddrPort("198.51.100.1:6881")

	// Expect BatchCommand with exactly one DropNode{ID: id} whose
	// Reason wraps errPeerIsReadOnly. Variadic on the mock side
	// appears as a single command in the args slice.
	table.On("BatchCommand", mock.MatchedBy(func(cmd ktable.Command) bool {
		drop, ok := cmd.(ktable.DropNode)
		return ok && drop.ID == id && errors.Is(drop.Reason, errPeerIsReadOnly)
	})).Return().Once()

	c.admitNodeFromReply(id, addr, true)

	table.AssertExpectations(t)
}

// TestAdmitNodeFromReply_notReadOnlyPuts — peer replied ro=0 → PutNode
// with the mandatory NodeResponded option and no extras.
func TestAdmitNodeFromReply_notReadOnlyPuts(t *testing.T) {
	t.Parallel()

	table := ktable_mocks.NewTable(t)
	c := &crawler{kTable: table}

	id := protocol.RandomNodeID()
	addr := netip.MustParseAddrPort("198.51.100.2:6881")

	table.On("BatchCommand", mock.MatchedBy(func(cmd ktable.Command) bool {
		put, ok := cmd.(ktable.PutNode)
		return ok && put.ID == id && put.Addr == addr && len(put.Options) == 1
	})).Return().Once()

	c.admitNodeFromReply(id, addr, false)

	table.AssertExpectations(t)
}

// TestAdmitNodeFromReply_extraOptionsAppendedAfterResponded — the
// sample_infohashes path supplies NodeBep51Support + NodeSampleInfoHashesRes
// as extras. Helper MUST emit NodeResponded first (so downstream
// bookkeeping sees "responded" before the BEP-51 flags) and append
// the caller's extras in order.
func TestAdmitNodeFromReply_extraOptionsAppendedAfterResponded(t *testing.T) {
	t.Parallel()

	table := ktable_mocks.NewTable(t)
	c := &crawler{kTable: table}

	id := protocol.RandomNodeID()
	addr := netip.MustParseAddrPort("198.51.100.3:6881")

	extra1 := ktable.NodeBep51Support(true)
	extra2 := ktable.NodeBep51Support(false) // dummy second extra for count

	table.On("BatchCommand", mock.MatchedBy(func(cmd ktable.Command) bool {
		put, ok := cmd.(ktable.PutNode)
		return ok && put.ID == id && put.Addr == addr && len(put.Options) == 3
	})).Return().Once()

	c.admitNodeFromReply(id, addr, false, extra1, extra2)

	table.AssertExpectations(t)
}
