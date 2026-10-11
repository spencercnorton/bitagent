package cataloguerecovery

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestDisabledAndExplicitBudgets(t *testing.T) {
	s, err := NewStore(nil, Config{})
	require.NoError(t, err)
	_, err = s.Remove(context.Background(), nil, "blocking", "", time.Time{})
	require.ErrorIs(t, err, ErrDisabled)
	_, err = s.Restore(context.Background(), 1, time.Now())
	require.ErrorIs(t, err, ErrDisabled)
	_, err = s.Filter(context.Background(), nil, nil)
	require.ErrorIs(t, err, ErrDisabled)
	good := Config{Enabled: true, Retention: time.Hour, MaxPayloadBytes: 1 << 20, MaxSnapshotBytes: 1 << 18, MaxSnapshots: 10, MaxStorageBytes: 8 << 20, MaxRowsPerSnapshot: 1000}
	require.NoError(t, good.Validate())
	cases := []Config{{Enabled: true}, good, good, good, good, good, good, good}
	cases[1].Retention = 0
	cases[2].MaxPayloadBytes = 0
	cases[3].MaxSnapshotBytes = 65 << 20
	cases[4].MaxSnapshots = 0
	cases[5].MaxStorageBytes = 0
	cases[6].MaxRowsPerSnapshot = 0
	cases[7].Retention = 366 * 24 * time.Hour
	for _, c := range cases {
		require.Error(t, c.Validate())
	}
	_, err = NewStore(nil, good)
	require.Error(t, err)
}
