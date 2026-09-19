package client

import (
	"context"
	"net/netip"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/spencercnorton/bitagent/internal/protocol"
)

type Client interface {
	Ping(ctx context.Context, addr netip.AddrPort) (PingResult, error)
	FindNode(ctx context.Context, addr netip.AddrPort, target protocol.ID) (FindNodeResult, error)
	GetPeers(ctx context.Context, addr netip.AddrPort, infoHash protocol.ID) (GetPeersResult, error)
	GetPeersScrape(ctx context.Context, addr netip.AddrPort, infoHash protocol.ID) (GetPeersScrapeResult, error)
	SampleInfoHashes(ctx context.Context, addr netip.AddrPort, target protocol.ID) (SampleInfoHashesResult, error)
}

// Every *Result here carries the peer's BEP-43 `ro` flag as
// `ReadOnly`. Callers that admit the replier to the routing table
// MUST consult this — a read-only peer must not be added. Previously
// the flag was silently dropped on the way up through serverAdapter,
// which meant the crawler admitted every responder regardless.

type PingResult struct {
	ID       protocol.ID
	ReadOnly bool
}

type FindNodeResult struct {
	ID       protocol.ID
	Nodes    []NodeInfo
	ReadOnly bool
}

type GetPeersResult struct {
	ID       protocol.ID
	Values   []netip.AddrPort
	Nodes    []NodeInfo
	ReadOnly bool
}

type GetPeersScrapeResult struct {
	ID        protocol.ID
	Values    []netip.AddrPort
	Nodes     []NodeInfo
	BfPeers   bloom.BloomFilter
	BfSeeders bloom.BloomFilter
	ReadOnly  bool
}

type SampleInfoHashesResult struct {
	ID       protocol.ID
	Samples  []protocol.ID
	Nodes    []NodeInfo
	Num      int
	Interval int
	ReadOnly bool
}

type NodeInfo struct {
	ID   protocol.ID
	Addr netip.AddrPort
}
