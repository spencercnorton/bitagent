package llmcapture

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type captureExpiryStoreStub struct {
	calls atomic.Int64
	seen  chan struct{}
}

func (s *captureExpiryStoreStub) DeleteExpired(
	context.Context,
) (int64, error) {
	s.calls.Add(1)
	select {
	case s.seen <- struct{}{}:
	default:
	}
	return 1, nil
}

func TestCaptureExpiryJanitorRunsWhenCaptureIsDisabled(t *testing.T) {
	cfg := NewDefaultConfig()
	require.False(t, cfg.Enabled)
	cfg.CleanupInterval = time.Hour
	store := &captureExpiryStoreStub{seen: make(chan struct{}, 1)}
	janitor, err := newJanitor(cfg, store, zap.NewNop().Sugar())
	require.NoError(t, err)
	require.NoError(t, janitor.Start(context.Background()))
	t.Cleanup(func() {
		require.NoError(t, janitor.Stop(context.Background()))
	})

	select {
	case <-store.seen:
	case <-time.After(time.Second):
		require.Fail(t, "disabled-capture janitor did not run at startup")
	}
	require.Equal(t, int64(1), store.calls.Load())
}

func TestCaptureExpiryJanitorRejectsInvalidInterval(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.CleanupInterval = 0
	_, err := newJanitor(
		cfg,
		&captureExpiryStoreStub{seen: make(chan struct{}, 1)},
		zap.NewNop().Sugar(),
	)
	require.ErrorContains(t, err, "cleanup_interval must be positive")
}
