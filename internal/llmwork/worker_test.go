package llmwork

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestInactiveOwnedWorkerNeverInitializesColdDatabaseOrRunsHandler(t *testing.T) {
	cfg := NewDefaultConfig()
	p := lazy.New(func() (*pgxpool.Pool, error) { t.Fatal("disabled worker initialized the database"); return nil, nil })
	s, err := NewStore(cfg, p)
	require.NoError(t, err)
	w, err := NewWorker(s, handlerFunc(func(context.Context, Task) error { t.Fatal("disabled worker dispatched a task"); return nil }), zap.NewNop().Sugar())
	require.NoError(t, err)
	require.NoError(t, w.Start(context.Background()))
	require.Error(t, w.Start(context.Background()))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Stop(ctx))
	require.NoError(t, w.Stop(ctx))
}
