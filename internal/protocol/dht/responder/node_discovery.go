package responder

import (
	"context"
	"time"

	"github.com/spencercnorton/bitagent/internal/protocol/dht"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
)

// responderNodeDiscovery attempts to add nodes from incoming requests to the discovered nodes channel.
type responderNodeDiscovery struct {
	responder       Responder
	discoveredNodes chan<- ktable.Node
}

func (r responderNodeDiscovery) Respond(ctx context.Context, msg dht.RecvMsg) (dht.Return, error) {
	ret, err := r.responder.Respond(ctx, msg)
	// BEP-43 §2: "Read-only nodes must not be added to the routing table,
	// and not returned in replies to find_node requests." A sender that
	// flags itself ro:1 will never answer our outbound queries, so adding
	// it here only inflates drop-rate and wastes outbound RTTs.
	if err == nil && !msg.Msg.ReadOnly {
		go func() {
			// wait for up to a second
			cancelCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			select {
			case <-cancelCtx.Done():
			case r.discoveredNodes <- ktable.NewNode(msg.Msg.A.ID, msg.From):
			}
		}()
	}

	return ret, err
}
