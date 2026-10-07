package serving_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
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

func TestServingPrivacyAcrossNativeConsumersIndependentOfOptionalPolicies(t *testing.T) {
	ctx := context.Background()
	pool, q := newServingPostgresDatabase(t, ctx)
	hashes := make([]protocol.ID, 9)
	for i := range hashes {
		hashes[i] = protocol.ID{byte(150 + i)}
		_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,$2,4096,$3,'multi',1,now(),now());
INSERT INTO torrent_contents(info_hash,content_type,size,is_anime,created_at,updated_at) VALUES($1,'movie',4096,false,now(),now());
INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,0,'Synthetic.mkv',4096,now(),now());
INSERT INTO torrent_tags(info_hash,name,created_at,updated_at) VALUES($1,$4,now(),now())`, pgx.QueryExecModeSimpleProtocol, hashes[i].Bytes(), fmt.Sprintf("Synthetic.Privacy.%d.2031.mkv", i), i == 0, fmt.Sprintf("privacy-test-%d", i))
		require.NoError(t, err)
	}
	for i, category := range []string{" \tPRIVATE\u00a0", " \nBITGRAB\r"} {
		_, err := pool.Exec(ctx, `INSERT INTO label_evidence(info_hash,source,source_kind,source_instance,source_object_id,category,observed_at,strength) VALUES($1,$4,'synthetic','fixture',$2,$3,now(),10)`, hashes[i+1].Bytes(), fmt.Sprint(i), category, " \tQBittorrent\r\n")
		require.NoError(t, err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO torrent_tags(info_hash,name,created_at,updated_at) VALUES($1,'bitgrab-synthetic',now(),now()),($2,'wanted-synthetic',now(),now()),($3,'bitgrabbed',now(),now()),($4,'manual-private',now(),now());
INSERT INTO torrent_canonical_labels(info_hash,category,resolved_source,resolved_strength,resolved_at) VALUES($5,$6,'synthetic',10,now())`, pgx.QueryExecModeSimpleProtocol, hashes[3].Bytes(), hashes[5].Bytes(), hashes[6].Bytes(), hashes[7].Bytes(), hashes[4].Bytes(), " \tPRIVATE\u00a0")
	require.NoError(t, err)
	before := func() string {
		var s string
		require.NoError(t, pool.QueryRow(ctx, `SELECT jsonb_build_object('raw',(SELECT jsonb_agg(to_jsonb(t) ORDER BY info_hash) FROM torrents t),'contents',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM torrent_contents t),'files',(SELECT jsonb_agg(to_jsonb(t) ORDER BY info_hash) FROM torrent_files t),'tags',(SELECT jsonb_agg(to_jsonb(t) ORDER BY info_hash,name) FROM torrent_tags t),'evidence',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM label_evidence t),'labels',(SELECT jsonb_agg(to_jsonb(t) ORDER BY info_hash) FROM torrent_canonical_labels t))::text`).Scan(&s))
		return s
	}
	snapshot := before()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("name-policy=%v", enabled), func(t *testing.T) {
			names, e := namepolicy.New(namepolicy.Config{Enabled: enabled})
			require.NoError(t, e)
			strong, media, e := classifier.CoreAdultServingEvidence()
			require.NoError(t, e)
			p, e := serving.NewPolicy(serving.Config{}, strong, media, names)
			require.NoError(t, e)
			s, e := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return q, nil }), ServingPolicy: p}).ServingSearch.Get()
			require.NoError(t, e)
			for i, h := range hashes {
				want := 1
				if i < 5 {
					want = 0
				}
				raw, e := s.TorrentsWithMissingInfoHashes(ctx, []protocol.ID{h})
				require.NoError(t, e)
				require.Len(t, raw.Torrents, want)
				contents, e := s.TorrentContent(ctx, query.WithTotalCount(true), query.WithAggregationBudget(0), query.Where(search.TorrentContentInfoHashCriteria(h)))
				require.NoError(t, e)
				require.Len(t, contents.Items, want)
				require.EqualValues(t, want, contents.TotalCount)
				files, e := s.TorrentFiles(ctx, query.WithTotalCount(true), query.WithAggregationBudget(0), query.Where(search.TorrentFileInfoHashCriteria(h)))
				require.NoError(t, e)
				require.Len(t, files.Items, want)
				require.EqualValues(t, want, files.TotalCount)
			}
			tags, e := s.TorrentSuggestTags(ctx, search.SuggestTagsQuery{Prefix: "privacy-test"})
			require.NoError(t, e)
			require.Len(t, tags.Suggestions, 4)
			gin.SetMode(gin.ReleaseMode)
			engine := gin.New()
			es := gql.NewExecutableSchema(gql.Config{Resolvers: &resolvers.Resolver{Search: s, Dao: q}})
			require.NoError(t, gqlhttp.New(gqlhttp.Params{Schema: lazy.New(func() (graphql.ExecutableSchema, error) { return es, nil })}).Option.Apply(engine))
			require.NoError(t, torznabhttp.New(lazy.New(func() (torznab.Client, error) {
				return adapter.NewWithFilters(s, nil, false, nil, adapter.FreshnessConfig{}), nil
			}), torznab.NewDefaultConfig(), nil).Apply(engine))
			body, _ := json.Marshal(map[string]string{"query": `{torrentContent{search(input:{limit:32,totalCount:true,aggregationBudget:0}){totalCount items{infoHash torrent{name}}}}}`})
			req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code)
			var response map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			require.NotContains(t, response, "errors")
			result := response["data"].(map[string]any)["torrentContent"].(map[string]any)["search"].(map[string]any)
			require.Equal(t, float64(4), result["totalCount"])
			require.Len(t, result["items"], 4)
			w = httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/torznab/api?t=search&cat=2000", nil))
			require.Equal(t, 200, w.Code)
			for i, h := range hashes {
				if i < 5 {
					require.NotContains(t, w.Body.String(), h.String())
				} else {
					require.Contains(t, w.Body.String(), h.String())
				}
			}
		})
	}
	require.Equal(t, snapshot, before(), "mandatory privacy hides consumer results without altering any source, metadata or authority row")
}
