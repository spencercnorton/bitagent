package cataloguerecovery_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPostgresClassifierDeleteListAndRestore(t *testing.T) {
	pool := recoveryPool(t)
	s := store(t, pool)
	ctx := context.Background()

	// 1. Insert a torrent using the helper
	h1 := hash(99)
	seed(t, pool, h1, true)

	// 2. Remove it using RemoveBatch with 'classifier_delete'
	snaps, err := s.RemoveBatch(ctx, [][]byte{h1}, "classifier_delete", `{"workflow":"test"}`, time.Now())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	// 3. List the classifier deletes
	deletes, total, err := s.ListClassifierDeletes(ctx, 10, 0)
	require.NoError(t, err)
	require.GreaterOrEqual(t, total, 1)

	found := false
	for _, d := range deletes {
		if string(d.InfoHash) == string(h1) {
			found = true
			require.Equal(t, `{"workflow":"test"}`, d.Reason)
		}
	}
	require.True(t, found, "classifier delete should be listed")

	// 4. Restore
	restored, err := s.Restore(ctx, snaps[0].ID, time.Now())
	require.NoError(t, err)
	require.True(t, restored)

	// 5. Ensure it's back in torrents
	var count int
	err = pool.QueryRow(ctx, `select count(*) from torrents where info_hash=$1`, h1).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "torrent should be restored")
}

func TestPostgresRecoveryRejectsForgedMechanism(t *testing.T) {
	pool := recoveryPool(t)
	s := store(t, pool)
	ctx := context.Background()

	h1 := hash(100)
	seed(t, pool, h1, true)

	// Remove it using a fake mechanism
	snaps, err := s.RemoveBatch(ctx, [][]byte{h1}, "content_filter", `{"workflow":"test"}`, time.Now())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	// Attempt to restore it - should fail because 'fake_mechanism' is not allowed for recovery
	restored, err := s.Restore(ctx, snaps[0].ID, time.Now())
	require.Error(t, err, "restoring a snapshot with a fake mechanism should fail")
	require.False(t, restored)
}
