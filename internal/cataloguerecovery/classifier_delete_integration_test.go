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

	// 1. Insert a torrent
	h1 := hash(99)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `insert into torrents(info_hash, name, size, files_status, discovered_at) values ($1, 'test_classifier_delete', 100, 'single', now())`, h1)
	require.NoError(t, err)
	err = tx.Commit(ctx)
	require.NoError(t, err)

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
