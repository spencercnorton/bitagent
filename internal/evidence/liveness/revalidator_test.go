package liveness

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/client"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
)

// stubTable returns a fixed list of known closest nodes. ID is
// arbitrary — the revalidator does not look at IDs.
type stubTable struct {
	nodes []ktable.Node
}

func (s stubTable) GetClosestNodes(_ ktable.ID) []ktable.Node {
	return s.nodes
}

// stubDHT returns canned responses keyed by peer address. err is
// returned for any address not present in responses.
type stubDHT struct {
	responses map[netip.AddrPort]client.GetPeersResult
	err       error
}

func (s stubDHT) GetPeers(_ context.Context, addr netip.AddrPort, _ protocol.ID) (client.GetPeersResult, error) {
	if s.err != nil {
		return client.GetPeersResult{}, s.err
	}
	if r, ok := s.responses[addr]; ok {
		return r, nil
	}
	return client.GetPeersResult{}, errors.New("no canned response")
}

// rearmTracker records every RearmRevalidate call.
type rearmTracker struct {
	calls int
}

func (t *rearmTracker) RearmRevalidate(_ context.Context, _ []byte, _ time.Duration) error {
	t.calls++
	return nil
}

// reviveTracker records every MarkAliveFromDHT call.
type reviveTracker struct {
	calls int
	err   error
}

func (t *reviveTracker) MarkAliveFromDHT(_ context.Context, _ []byte) error {
	t.calls++
	return t.err
}

func mkAddr(port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{1, 2, 3, byte(port)}), port)
}

func mkNode(port uint16) ktable.Node {
	var id protocol.ID
	id[0] = byte(port)
	return ktable.NewNode(id, mkAddr(port))
}

// TestRevalidateAlivePromotes — when the DHT returns peers above
// the floor, the resolver is invoked to mark alive and the rearm
// path is NOT taken.
func TestRevalidateAlivePromotes(t *testing.T) {
	cfg := evidence.NewDefaultLivenessConfig()
	cfg.Enabled = true
	cfg.RevalidateMinPeers = 2

	node1 := mkNode(11)
	node2 := mkNode(22)
	dht := stubDHT{responses: map[netip.AddrPort]client.GetPeersResult{
		node1.Addr(): {Values: []netip.AddrPort{mkAddr(101), mkAddr(102)}},
		node2.Addr(): {Values: []netip.AddrPort{mkAddr(103)}},
	}}

	table := stubTable{nodes: []ktable.Node{node1, node2}}
	store := &rearmTracker{}
	rev := &reviveTracker{}
	metrics := NewMetrics()

	ih := []byte("aaaaaaaaaaaaaaaaaaaa")
	revalidateOneWith(context.Background(), ih, cfg, table, dht, store, rev, metrics)

	if rev.calls != 1 {
		t.Fatalf("expected 1 mark-alive call, got %d", rev.calls)
	}
	if store.calls != 0 {
		t.Fatalf("rearm should not run when peer floor met, got %d", store.calls)
	}
}

// TestRevalidateBelowFloorRearms — when the swarm returns fewer
// peers than the floor, the row's TTL is bumped and no resolver
// call is made.
func TestRevalidateBelowFloorRearms(t *testing.T) {
	cfg := evidence.NewDefaultLivenessConfig()
	cfg.Enabled = true
	cfg.RevalidateMinPeers = 2

	node1 := mkNode(11)
	dht := stubDHT{responses: map[netip.AddrPort]client.GetPeersResult{
		node1.Addr(): {Values: []netip.AddrPort{mkAddr(101)}}, // only 1 peer
	}}

	table := stubTable{nodes: []ktable.Node{node1}}
	store := &rearmTracker{}
	rev := &reviveTracker{}
	metrics := NewMetrics()

	ih := []byte("bbbbbbbbbbbbbbbbbbbb")
	revalidateOneWith(context.Background(), ih, cfg, table, dht, store, rev, metrics)

	if rev.calls != 0 {
		t.Fatalf("expected no mark-alive call, got %d", rev.calls)
	}
	if store.calls != 1 {
		t.Fatalf("expected 1 rearm call, got %d", store.calls)
	}
}

// TestRevalidateNoNodesRearmsSilently — if the routing table has no
// closest nodes for the hash, we never queried anyone, so this is
// not a "still dead" verdict; it's a rearm-and-move-on.
func TestRevalidateNoNodesRearmsSilently(t *testing.T) {
	cfg := evidence.NewDefaultLivenessConfig()
	cfg.Enabled = true

	dht := stubDHT{}
	table := stubTable{} // empty
	store := &rearmTracker{}
	rev := &reviveTracker{}
	metrics := NewMetrics()

	ih := []byte("cccccccccccccccccccc")
	revalidateOneWith(context.Background(), ih, cfg, table, dht, store, rev, metrics)

	if rev.calls != 0 {
		t.Fatalf("expected no mark-alive call, got %d", rev.calls)
	}
	if store.calls != 1 {
		t.Fatalf("expected 1 rearm call, got %d", store.calls)
	}
}

// TestRevalidateDedupesPeers — if multiple nodes return overlapping
// peer sets, the floor must be measured against the unique set, not
// the sum, otherwise a single popular peer reported by N nodes
// would falsely revive a dead torrent.
func TestRevalidateDedupesPeers(t *testing.T) {
	cfg := evidence.NewDefaultLivenessConfig()
	cfg.Enabled = true
	cfg.RevalidateMinPeers = 2

	node1 := mkNode(11)
	node2 := mkNode(22)
	// Both responders return the same single peer.
	common := mkAddr(101)
	dht := stubDHT{responses: map[netip.AddrPort]client.GetPeersResult{
		node1.Addr(): {Values: []netip.AddrPort{common}},
		node2.Addr(): {Values: []netip.AddrPort{common}},
	}}

	table := stubTable{nodes: []ktable.Node{node1, node2}}
	store := &rearmTracker{}
	rev := &reviveTracker{}
	metrics := NewMetrics()

	ih := []byte("dddddddddddddddddddd")
	revalidateOneWith(context.Background(), ih, cfg, table, dht, store, rev, metrics)

	if rev.calls != 0 {
		t.Fatalf("dedupe broke: 2 nodes reporting the same peer should not satisfy floor=2, got mark-alive=%d", rev.calls)
	}
	if store.calls != 1 {
		t.Fatalf("expected 1 rearm call, got %d", store.calls)
	}
}

// TestRevalidateBadHashLengthIsError — info_hash bytea must be 20
// bytes; anything else is a corruption signal that should not
// trigger a DHT call.
func TestRevalidateBadHashLengthIsError(t *testing.T) {
	cfg := evidence.NewDefaultLivenessConfig()
	cfg.Enabled = true

	dht := stubDHT{}
	table := stubTable{nodes: []ktable.Node{mkNode(11)}}
	store := &rearmTracker{}
	rev := &reviveTracker{}
	metrics := NewMetrics()

	revalidateOneWith(context.Background(), []byte("short"), cfg, table, dht, store, rev, metrics)

	if rev.calls != 0 || store.calls != 0 {
		t.Fatalf("bad hash should not produce DHT or store traffic, got rev=%d store=%d", rev.calls, store.calls)
	}
}
