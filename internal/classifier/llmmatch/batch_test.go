package llmmatch

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func batchTorrents(n int) []model.Torrent {
	ts := make([]model.Torrent, 0, n)
	for i := 0; i < n; i++ {
		ts = append(ts, mediaTorrent(fmt.Sprintf("Some.Movie.%d.2020.1080p.WEB-DL", i)))
	}
	return ts
}

// batchItemsJSON builds a {"items":[...]} response for ids 1..n, skipping any
// ids in skip.
func batchItemsJSON(n int, skip ...int) string {
	skipSet := map[int]struct{}{}
	for _, s := range skip {
		skipSet[s] = struct{}{}
	}
	items := make([]map[string]any, 0, n)
	for id := 1; id <= n; id++ {
		if _, ok := skipSet[id]; ok {
			continue
		}
		items = append(items, map[string]any{
			"id": id, "title": fmt.Sprintf("Some Movie %d", id-1), "year": 2020, "type": "movie",
			"is_anime": false, "is_pack": false, "is_adult": false,
		})
	}
	raw, _ := json.Marshal(map[string]any{"items": items})
	return string(raw)
}

func TestExtractMany_SingleBatchCall(t *testing.T) {
	srv, calls := chatServer(t, batchItemsJSON(8))
	c := testClient(srv.URL)

	ts := batchTorrents(8)
	st := c.ExtractMany(context.Background(), ts, 8)

	assert.Equal(t, 1, int(atomic.LoadInt32(calls)))
	assert.Equal(t, 8, st.Requested)
	assert.Equal(t, 8, st.OK)
	assert.Equal(t, 1, st.Batches)
	assert.Zero(t, st.Empty+st.Failed+st.Singles+st.Cached)

	// The whole point: a subsequent Extract is a pure cache hit.
	ext, err := c.Extract(context.Background(), ts[3])
	require.NoError(t, err)
	assert.Equal(t, "Some Movie 3", ext.Title)
	assert.True(t, ext.OK)
	assert.Equal(t, 1, int(atomic.LoadInt32(calls)))
}

func TestExtractMany_NativePrivateBatchNeverCallsModel(t *testing.T) {
	srv, calls := chatServer(t, batchItemsJSON(8))
	c := testClient(srv.URL)

	ts := batchTorrents(8)
	for i := range ts {
		ts[i].Private = true
	}
	st := c.ExtractMany(context.Background(), ts, 8)

	assert.Zero(t, atomic.LoadInt32(calls))
	assert.Equal(t, 8, st.Requested)
	assert.Equal(t, 8, st.Gated)
	assert.Zero(t, st.OK+st.Empty+st.Failed+st.Batches+st.Singles+st.Cached)
}

func TestExtractMany_DroppedRowRetriesAsSingle(t *testing.T) {
	// Batch echoes 7 of 8 ids (drops id 3) — the count-match guardrail must
	// re-fetch exactly that name with a single extract call.
	srv, calls := chatServer(t,
		batchItemsJSON(8, 3),
		`{"title":"Some Movie 2","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
	)
	c := testClient(srv.URL)

	st := c.ExtractMany(context.Background(), batchTorrents(8), 8)

	assert.Equal(t, 2, int(atomic.LoadInt32(calls)))
	assert.Equal(t, 8, st.OK)
	assert.Equal(t, 1, st.Batches)
	assert.Equal(t, 1, st.Singles)
}

func TestExtractMany_UndecodableBatchSplitsToSingles(t *testing.T) {
	// First call is garbage; 8 splits into 4+4, both below minBatchSplit, so
	// every name falls back to a single extract: 1 batch + 8 singles.
	contents := []string{"not json at all"}
	for i := 0; i < 8; i++ {
		contents = append(contents, fmt.Sprintf(`{"title":"Some Movie %d","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`, i))
	}
	srv, calls := chatServer(t, contents...)
	c := testClient(srv.URL)

	st := c.ExtractMany(context.Background(), batchTorrents(8), 8)

	assert.Equal(t, 9, int(atomic.LoadInt32(calls)))
	assert.Equal(t, 8, st.OK)
	assert.Equal(t, 1, st.Batches)
	assert.Equal(t, 8, st.Singles)
	assert.Zero(t, st.Failed)
}

func TestExtractMany_SkipsCachedAndDuplicateNames(t *testing.T) {
	srv, calls := chatServer(t,
		`{"title":"Some Movie 0","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`,
		batchItemsJSON(8),
	)
	c := testClient(srv.URL)

	ts := batchTorrents(9)
	// Pre-warm one name via the single path, and append a duplicate of another.
	_, err := c.Extract(context.Background(), ts[0])
	require.NoError(t, err)
	ts = append(ts, mediaTorrent(ts[1].Name))

	st := c.ExtractMany(context.Background(), ts, 8)

	// 10 in, 1 cached + 1 duplicate skipped, 8 batched in one call.
	assert.Equal(t, 2, int(atomic.LoadInt32(calls)))
	assert.Equal(t, 10, st.Requested)
	assert.Equal(t, 2, st.Cached)
	assert.Equal(t, 8, st.OK)
	assert.Equal(t, 1, st.Batches)
}

func TestExtractMany_InventedIDsAreDropped(t *testing.T) {
	// Response invents id 99 and duplicates id 1; ids 2..8 are missing → 7
	// single fallbacks.
	items := `{"items":[{"id":1,"title":"Some Movie 0","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false},{"id":1,"title":"Dup","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false},{"id":99,"title":"Ghost","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}]}`
	contents := []string{items}
	for i := 1; i < 8; i++ {
		contents = append(contents, fmt.Sprintf(`{"title":"Some Movie %d","year":2020,"type":"movie","is_anime":false,"is_pack":false,"is_adult":false}`, i))
	}
	srv, calls := chatServer(t, contents...)
	c := testClient(srv.URL)

	st := c.ExtractMany(context.Background(), batchTorrents(8), 8)

	assert.Equal(t, 8, int(atomic.LoadInt32(calls)))
	assert.Equal(t, 8, st.OK)
	assert.Equal(t, 7, st.Singles)

	// The first-seen item for id 1 wins, not the duplicate.
	ext, err := c.Extract(context.Background(), batchTorrents(1)[0])
	require.NoError(t, err)
	assert.Equal(t, "Some Movie 0", ext.Title)
}
