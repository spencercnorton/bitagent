package seeds

// Owned PostgreSQL regression for the released selector. It starts no worker,
// scraper, DHT client or provider. Only the disposable seedsTestPool is written.

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/stretchr/testify/require"
)

func TestPostgresSelectStaleRechecksUnderContinuousNewBacklog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := seedsTestPool(t)
	names, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	store := NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }), names)
	add := func(id uint32, name string, private, stale bool) []byte {
		t.Helper()
		hash := make([]byte, 20)
		binary.BigEndian.PutUint32(hash[16:], id)
		_, err := pool.Exec(ctx, `insert into torrents(info_hash,name,private) values($1,$2,$3)`, hash, name, private)
		require.NoError(t, err)
		if stale {
			_, err = pool.Exec(ctx, `insert into torrent_tracker_seeds(info_hash,tracker_known,seeders,leechers,checked_at,last_positive_at) values($1,true,1,0,now()-interval '48 hours',now()-interval '48 hours')`, hash)
			require.NoError(t, err)
		}
		return hash
	}
	old := map[string]bool{}
	for i, tag := range []string{"wanted:fixture", "manual:fixture", "reference:fixture"} {
		h := add(uint32(i+1), "Synthetic.Recheck.Release", false, true)
		old[string(h)] = true
		_, err := pool.Exec(ctx, `insert into torrent_tags(info_hash,name) values($1,$2)`, h, tag)
		require.NoError(t, err)
	}
	// Both partitions must retain the current shared name/privacy/ANY-XXX guards.
	blocked := [][]byte{
		add(10, "Synthetic.Private.Release", true, true),
		add(11, "Synthetic.电影.Release", false, false),
		add(12, "Synthetic.Classified.Release", false, true),
		add(13, "Synthetic.QB.Release", false, false),
		add(14, "Synthetic.Tag.Release", false, true),
		add(15, "Synthetic.Canonical.Release", false, false),
	}
	_, err = pool.Exec(ctx, `insert into torrent_contents(id,info_hash,content_type) values('synthetic-xxx',$1,'xxx')`, blocked[2])
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `insert into label_evidence(info_hash,source,category) values($1,$2,$3)`, blocked[3], " QBittorrent\u00a0", " BitGrab\t")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `insert into torrent_tags(info_hash,name) values($1,'bitgrab:fixture')`, blocked[4])
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `insert into torrent_canonical_labels(info_hash,category) values($1,'private')`, blocked[5])
	require.NoError(t, err)

	const batch = 4
	next := uint32(100)
	addNewBatch := func() {
		for i := 0; i < batch; i++ {
			add(next, "Synthetic.New.Release", false, false)
			next++
		}
	}
	addNewBatch()
	revisited := map[string]bool{}
	for cycle := 0; cycle < 3; cycle++ {
		selected, err := store.SelectStale(ctx, 24*time.Hour, batch)
		require.NoError(t, err)
		require.Len(t, selected, batch)
		unique := map[string]bool{}
		outcomes := map[string]*ScrapeOutcome{}
		for _, h := range selected {
			require.False(t, unique[string(h)], "selector duplicated a hash")
			unique[string(h)] = true
			for _, denied := range blocked {
				require.NotEqual(t, string(denied), string(h), "selection bypassed current admission")
			}
			if old[string(h)] {
				revisited[string(h)] = true
			}
			outcomes[string(h)] = &ScrapeOutcome{TrackerKnown: true, Seeders: 1}
		}
		// Simulate successful persistence using the real writer, without a packet.
		_, _, err = store.Persist(ctx, selected, outcomes)
		require.NoError(t, err)
		addNewBatch()
	}
	require.Equal(t, 3, len(revisited), "never-checked backlog starved previously checked public rows across three completed cycles")
}

func TestPostgresSelectStaleReturnsUnusedClassCapacity(t *testing.T) {
	for _, tc := range []struct {
		name                string
		never, stale, limit int
		wantTotal, wantOld  int
	}{
		{"only_never", 6, 0, 4, 4, 0},
		{"only_stale", 0, 6, 4, 4, 4},
		{"one_stale", 6, 1, 4, 4, 1},
		{"one_never", 1, 6, 4, 4, 3},
		{"odd_batch", 6, 6, 5, 5, 2},
		{"single_preserves_priority", 6, 6, 1, 1, 0},
		{"short_catalogue", 1, 1, 4, 2, 1},
		{"zero_limit", 6, 6, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool := seedsTestPool(t)
			store := NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
			old := map[string]bool{}
			for i := 0; i < tc.never+tc.stale; i++ {
				hash := make([]byte, 20)
				binary.BigEndian.PutUint32(hash[16:], uint32(i+1))
				_, err := pool.Exec(ctx, `insert into torrents(info_hash) values($1)`, hash)
				require.NoError(t, err)
				if i >= tc.never {
					old[string(hash)] = true
					_, err = pool.Exec(ctx, `insert into torrent_tracker_seeds(info_hash,checked_at) values($1,now()-interval '48 hours')`, hash)
					require.NoError(t, err)
				}
			}
			selected, err := store.SelectStale(ctx, 24*time.Hour, tc.limit)
			require.NoError(t, err)
			require.Len(t, selected, tc.wantTotal)
			unique, oldCount := map[string]bool{}, 0
			for _, hash := range selected {
				require.False(t, unique[string(hash)])
				unique[string(hash)] = true
				if old[string(hash)] {
					oldCount++
				}
			}
			require.Equal(t, tc.wantOld, oldCount)
		})
	}
}

func TestPostgresSelectStaleRejectsNegativeBatchAndSkipsFresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := seedsTestPool(t)
	store := NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	hash := make([]byte, 20)
	_, err := pool.Exec(ctx, `insert into torrents(info_hash) values($1)`, hash)
	require.NoError(t, err)
	_, err = store.SelectStale(ctx, 24*time.Hour, -1)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `insert into torrent_tracker_seeds(info_hash,checked_at) values($1,now())`, hash)
	require.NoError(t, err)
	selected, err := store.SelectStale(ctx, 24*time.Hour, 4)
	require.NoError(t, err)
	require.Empty(t, selected)
}
