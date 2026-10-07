package serving_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/99designs/gqlgen/graphql"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/gql"
	gqlhttp "github.com/spencercnorton/bitagent/internal/gql/httpserver"
	"github.com/spencercnorton/bitagent/internal/gql/resolvers"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/serving"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"github.com/spencercnorton/bitagent/internal/torznab/adapter"
	torznabhttp "github.com/spencercnorton/bitagent/internal/torznab/httpserver"
	"github.com/stretchr/testify/require"
)

func TestServingPostgresNamePolicyUsesReleaseNameAcrossHashContentFilesAndTags(t *testing.T) {
	ctx := context.Background()
	pool, q := newServingPostgresDatabase(t, ctx)
	names := []string{"Synthetic.测试.2026.mkv", "Synthetic.Фильм.2026.mkv", "Synthetic.Fetish.XXX.2026.mkv", "xXx.2002.1080p.mkv", "Amélie.2001.mkv", "Synthetic.테스트.2026.mkv", "Synthetic.Fetish.2005.Drama.mkv", "Synthetic.Ordinary.2026.mkv"}
	hashes := make([]protocol.ID, len(names))
	for i, name := range names {
		hashes[i] = protocol.ID{byte(90 + i)}
		_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,$2,4096,false,'multi',1,now(),now());
INSERT INTO torrent_contents(info_hash,content_type,size,is_anime,created_at,updated_at) VALUES($1,'movie',4096,false,now(),now());
INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,0,'Support.字幕.srt',4096,now(),now());
INSERT INTO torrent_tags(info_hash,name,created_at,updated_at) VALUES($1,$3,now(),now())`, pgx.QueryExecModeSimpleProtocol, hashes[i].Bytes(), name, fmt.Sprint("synthetic-tag-", i))
		require.NoError(t, err)
	}
	policy, err := namepolicy.New(namepolicy.Config{Enabled: true, ExcludedInfoHashes: []string{hashes[7].String()}})
	require.NoError(t, err)
	strong, media, err := classifier.CoreAdultServingEvidence()
	require.NoError(t, err)
	servingPolicy, err := serving.NewPolicy(serving.Config{ExcludeAdult: true}, strong, media, policy)
	require.NoError(t, err)
	consumer, err := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return q, nil }), ServingPolicy: servingPolicy}).ServingSearch.Get()
	require.NoError(t, err)
	for i, hash := range hashes {
		want := 1
		if i < 3 || i == 7 {
			want = 0
		}
		raw, err := consumer.TorrentsWithMissingInfoHashes(ctx, []protocol.ID{hash})
		require.NoError(t, err)
		require.Len(t, raw.Torrents, want)
		content, err := consumer.TorrentContent(ctx, query.WithTotalCount(true), query.WithAggregationBudget(0), query.Where(search.TorrentContentInfoHashCriteria(hash)))
		require.NoError(t, err)
		require.Len(t, content.Items, want)
		require.EqualValues(t, want, content.TotalCount)
		files, err := consumer.TorrentFiles(ctx, query.WithTotalCount(true), query.WithAggregationBudget(0), query.Where(search.TorrentFileInfoHashCriteria(hash)))
		require.NoError(t, err)
		require.Len(t, files.Items, want, "support-file script is not release-name policy")
		require.EqualValues(t, want, files.TotalCount)
	}
	var rawCount, fileCount, contentsCount, tagCount int
	tags, err := consumer.TorrentSuggestTags(ctx, search.SuggestTagsQuery{Prefix: "synthetic-tag"})
	require.NoError(t, err)
	require.Len(t, tags.Suggestions, 4)
	for _, tag := range tags.Suggestions {
		for _, i := range []int{0, 1, 2, 7} {
			require.NotEqual(t, fmt.Sprint("synthetic-tag-", i), tag.Name)
		}
	}
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	es := gql.NewExecutableSchema(gql.Config{Resolvers: &resolvers.Resolver{Search: consumer, Dao: q}})
	require.NoError(t, gqlhttp.New(gqlhttp.Params{Schema: lazy.New(func() (graphql.ExecutableSchema, error) { return es, nil })}).Option.Apply(engine))
	client := adapter.NewWithFilters(consumer, nil, false, nil, adapter.FreshnessConfig{})
	require.NoError(t, torznabhttp.New(lazy.New(func() (torznab.Client, error) { return client, nil }), torznab.NewDefaultConfig(), nil).Apply(engine))
	body, _ := json.Marshal(map[string]string{"query": `{torrentContent{search(input:{limit:32,totalCount:true,aggregationBudget:0}){totalCount items{infoHash torrent{name}}}}}`})
	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var response map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.NotContains(t, response, "errors")
	got := response["data"].(map[string]any)["torrentContent"].(map[string]any)["search"].(map[string]any)
	require.Equal(t, float64(4), got["totalCount"])
	require.Len(t, got["items"], 4)
	w = httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/torznab/api?t=search&cat=2000", nil).WithContext(ctx))
	require.Equal(t, http.StatusOK, w.Code)
	for _, i := range []int{0, 1, 2, 7} {
		require.NotContains(t, w.Body.String(), hashes[i].String())
	}
	require.Contains(t, w.Body.String(), hashes[3].String())
	require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM torrents),(SELECT count(*) FROM torrent_files),(SELECT count(*) FROM torrent_contents),(SELECT count(*) FROM torrent_tags)`).Scan(&rawCount, &fileCount, &contentsCount, &tagCount))
	require.Equal(t, len(names), rawCount)
	require.Equal(t, len(names), fileCount)
	require.Equal(t, len(names), contentsCount)
	require.Equal(t, len(names), tagCount)
}
