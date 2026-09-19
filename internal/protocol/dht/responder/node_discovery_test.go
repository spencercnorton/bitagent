package responder

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/dht"
	"github.com/spencercnorton/bitagent/internal/protocol/dht/ktable"
	"github.com/stretchr/testify/assert"
)

// stubResponder lets the test control the error returned by the inner
// responder without wiring a full mock.
type stubResponder struct {
	err error
}

func (s stubResponder) Respond(_ context.Context, _ dht.RecvMsg) (dht.Return, error) {
	return dht.Return{}, s.err
}

func newRecvMsg(readOnly bool) dht.RecvMsg {
	return dht.RecvMsg{
		From: dht.RandomNodeInfo(4).Addr.ToAddrPort(),
		Msg: dht.Msg{
			ReadOnly: readOnly,
			A: &dht.MsgArgs{
				ID: protocol.RandomNodeID(),
			},
		},
	}
}

// assertNodeDiscoveryPush runs Respond and reports whether a node was pushed
// onto discoveredNodes within the given timeout. The handler pushes in a
// goroutine so we poll the channel rather than relying on ordering.
func assertNodeDiscoveryPush(
	t *testing.T,
	innerErr error,
	msg dht.RecvMsg,
	wait time.Duration,
) bool {
	t.Helper()

	discovered := make(chan ktable.Node, 1)
	r := responderNodeDiscovery{
		responder:       stubResponder{err: innerErr},
		discoveredNodes: discovered,
	}

	_, err := r.Respond(context.Background(), msg)
	assert.ErrorIs(t, err, innerErr)

	select {
	case <-discovered:
		return true
	case <-time.After(wait):
		return false
	}
}

func TestResponderNodeDiscovery_pushesOnNormalMessage(t *testing.T) {
	t.Parallel()

	pushed := assertNodeDiscoveryPush(t, nil, newRecvMsg(false), time.Second)
	assert.True(t, pushed, "non-read-only sender should be added to discovered nodes")
}

func TestResponderNodeDiscovery_skipsReadOnlySender(t *testing.T) {
	t.Parallel()

	pushed := assertNodeDiscoveryPush(t, nil, newRecvMsg(true), 250*time.Millisecond)
	assert.False(t, pushed, "BEP-43 read-only sender must not be added to the routing table")
}

func TestResponderNodeDiscovery_skipsOnResponderError(t *testing.T) {
	t.Parallel()

	// Pre-existing contract: if the inner responder errors, we do not
	// advertise the sender as a healthy node. Preserved by this MR.
	pushed := assertNodeDiscoveryPush(
		t,
		errors.New("boom"),
		newRecvMsg(false),
		250*time.Millisecond,
	)
	assert.False(t, pushed, "error response should suppress node discovery push")
}
