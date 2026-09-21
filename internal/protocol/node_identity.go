package protocol

import "net/netip"

// NodeIdentity is the output of the BEP-42 node ID pipeline: the ID the
// DHT stack uses, the external IP it was derived from (zero when the
// pipeline fell back to a random ID) and whether that fallback is active.
type NodeIdentity struct {
	ID             ID
	ExternalIP     netip.Addr
	RandomFallback bool
}
