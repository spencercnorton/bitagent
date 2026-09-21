package dhtfx

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/externalip"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type countingResolver struct{ calls atomic.Int32 }

func (r *countingResolver) Resolve(context.Context) (netip.Addr, error) {
	r.calls.Add(1)
	return netip.MustParseAddr("203.0.113.5"), nil
}

// TestNodeIdentityIsLazy pins the quiet-startup contract: constructing the
// module performs no external-IP lookup; the first Get resolves exactly once
// and yields a BEP-42 ID for that IP.
func TestNodeIdentityIsLazy(t *testing.T) {
	resolver := &countingResolver{}
	res := provideNodeIdentity(identityParams{
		Config:   externalip.NewDefaultFxConfig(),
		Resolver: resolver,
		Metrics:  externalip.NewWatcherMetrics(),
		Logger:   zap.NewNop().Sugar(),
	})
	require.Equal(t, int32(0), resolver.calls.Load())

	id, err := res.Identity.Get()
	require.NoError(t, err)
	require.Equal(t, int32(1), resolver.calls.Load())
	require.False(t, id.RandomFallback)
	require.True(t, protocol.VerifySecureNodeID(id.ID, netip.MustParseAddr("203.0.113.5")))

	_, _ = res.Identity.Get()
	require.Equal(t, int32(1), resolver.calls.Load())
	require.NoError(t, res.AppHook.OnStop(context.Background()))
}
