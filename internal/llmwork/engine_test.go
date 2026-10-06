package llmwork

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeBackend struct {
	lease         Lease
	claims        int
	state, reason string
	retry         time.Time
	completed     bool
}

func (b *fakeBackend) Claim(context.Context, string) (*Lease, error) {
	b.claims++
	return &b.lease, nil
}
func (b *fakeBackend) Finish(_ context.Context, _ Lease, state, reason string, retry time.Time) error {
	b.state, b.reason, b.retry = state, reason, retry
	return nil
}
func (b *fakeBackend) Heartbeat(context.Context, Lease) error         { return nil }
func (b *fakeBackend) Completed(context.Context, Lease) (bool, error) { return b.completed, nil }

type handlerFunc func(context.Context, Task) error

func (f handlerFunc) Handle(ctx context.Context, t Task) error { return f(ctx, t) }

func TestEngineKeepsAdmissionAndUncertainDispatchDistinct(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		state string
	}{
		{"budget", DeferredError{"allowance", time.Date(2031, 1, 2, 0, 0, 0, 0, time.UTC)}, "deferred"},
		{"recorded_budget", &DeferredError{"allowance", time.Date(2031, 1, 2, 0, 0, 0, 0, time.UTC)}, "deferred"},
		{"abstained", DeclinedError{Reason: "type_not_qualified"}, "obsolete"},
		{"privacy", ErrObsolete, "obsolete"}, {"unknown_dispatch", ErrHeld, "held"}, {"cancellation", context.Canceled, "held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBackend{}
			cfg := NewDefaultConfig()
			cfg.Enabled = true
			e := Engine{Backend: b, Config: cfg, Owner: "test", Handler: handlerFunc(func(ctx context.Context, _ Task) error { require.NotNil(t, ExecutionFrom(ctx)); return tc.err })}
			worked, err := e.RunOne(context.Background())
			require.NoError(t, err)
			require.True(t, worked)
			require.Equal(t, tc.state, b.state)
			if errors.Is(tc.err, ErrDeferred) {
				require.Equal(t, time.UTC, b.retry.Location())
				require.Equal(t, 2, b.retry.Day())
			}
		})
	}
}

func TestDisabledWorkerDoesNotClaimOrRun(t *testing.T) {
	b := &fakeBackend{}
	e := Engine{Backend: b, Config: NewDefaultConfig(), Owner: "test", Handler: handlerFunc(func(context.Context, Task) error { t.Fatal("disabled worker ran"); return nil })}
	worked, err := e.RunOne(context.Background())
	require.NoError(t, err)
	require.False(t, worked)
	require.Zero(t, b.claims)
}

func TestEngineRequiresCommittedCompletion(t *testing.T) {
	for _, committed := range []bool{false, true} {
		b := &fakeBackend{completed: committed}
		cfg := NewDefaultConfig()
		cfg.Enabled = true
		e := Engine{Backend: b, Config: cfg, Owner: "test", Handler: handlerFunc(func(context.Context, Task) error { return nil })}
		worked, err := e.RunOne(context.Background())
		require.NoError(t, err)
		require.True(t, worked)
		if committed {
			require.Empty(t, b.state)
		} else {
			require.Equal(t, "held", b.state)
		}
	}
}
