package search

import (
	"context"
	"testing"

	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/serving"
	"github.com/stretchr/testify/require"
)

func TestGroupedPrefixPostgresSharedNamePolicyEnabled(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	// Actual owner identifiers never enter this synthetic public regression.
	for index, name := range map[int]string{
		2: "Synthetic.电影.ENG.mkv",
		3: "Synthetic.Фильм.English.mkv",
		5: "Fetish.XXX.Synthetic.mkv",
		7: "Synthetic.Ordinary.Positive.2031.mkv",
	} {
		_, err := f.pool.Exec(ctx, `update torrents set name=$2 where info_hash=$1`, f.hashes[index].Bytes(), name)
		require.NoError(t, err)
	}
	_, err := f.pool.Exec(ctx, `insert into torrent_contents
  (id,info_hash,content_type,content_source,content_id,seeders)
values ('synthetic-policy-positive',$1,'movie','synthetic','ordinary',80)`, f.hashes[7].Bytes())
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `insert into torrent_tags(info_hash,name) values($1,'manual'),($1,'wanted')`, f.hashes[7].Bytes())
	require.NoError(t, err)
	before := f.snapshot(t)
	f, trace := observeGroupedPrefixFixture(t, f)
	for _, enabled := range []bool{false, true} {
		names, err := namepolicy.New(namepolicy.Config{Enabled: enabled,
			ExcludedInfoHashes: []string{f.hashes[4].String()}})
		require.NoError(t, err)
		policy, err := serving.NewPolicy(serving.Config{ExcludeAdult: true}, []string{"siterip"}, []string{"mkv", "jpg"}, names)
		require.NoError(t, err)
		s := &search{q: f.q, adultPolicy: policy}
		result := requireGroupedPrefixEquivalent(t, s,
			query.OrderBy(append(TorrentContentOrderByName.Clauses(OrderDirectionAscending),
				TorrentContentOrderByInfoHash.Clauses(OrderDirectionDescending)...)...),
			query.Limit(1), query.WithHasNextPage(false),
			query.WithFacet(TorrentContentTypeFacet(query.FacetIsAggregated())))
		require.EqualValues(t, 1, result.TotalCount)
		require.NotNil(t, result.Aggregations[TorrentContentTypeFacetKey])
		if enabled {
			require.Equal(t, f.hashes[7], result.Items[0].InfoHash)
			require.EqualValues(t, 1, result.Aggregations[TorrentContentTypeFacetKey].Items["movie"].Count)
			raw, e := s.Torrents(ctx, adultServingExactOptions(model.TableNameTorrent)...)
			require.NoError(t, e)
			require.EqualValues(t, 2, raw.TotalCount)
			require.Equal(t, f.hashes[6], raw.Items[0].InfoHash)
			require.Equal(t, f.hashes[7], raw.Items[1].InfoHash)
		}
	}
	require.Positive(t, trace.prefixes.Load(), "shared policy and existing adult/quarantine guards must execute together on the prefix path")
	require.Equal(t, before, f.snapshot(t), "name decisions cannot rewrite protected tags, catalogue identities or quarantine snapshots")
}
