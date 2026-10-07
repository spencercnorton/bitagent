package serving_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/gql"
	gqlhttp "github.com/spencercnorton/bitagent/internal/gql/httpserver"
	"github.com/spencercnorton/bitagent/internal/gql/resolvers"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
	"github.com/spencercnorton/bitagent/internal/junkpurge/quarantinehttp"
	"github.com/spencercnorton/bitagent/internal/keywords"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/serving"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"github.com/spencercnorton/bitagent/internal/torznab/adapter"
	torznabhttp "github.com/spencercnorton/bitagent/internal/torznab/httpserver"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func newServingPostgresDatabase(t *testing.T, ctx context.Context) (*pgxpool.Pool, *dao.Query) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to disposable PostgreSQL")
	}
	schema := fmt.Sprintf("serving_api_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() {
		_ = db.Close()
		pool.Close()
		cleanup, e := pgx.Connect(context.Background(), dsn)
		if e == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)
	return pool, dao.Use(gdb)
}

func TestServingPostgresTypedAdultSkipsExpensiveSubplans(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, q := newServingPostgresDatabase(t, ctx)
	_, err := pool.Exec(ctx, `
INSERT INTO torrents (info_hash, name, size, private, files_status, created_at, updated_at)
SELECT decode(md5(i::text), 'hex'), 'Synthetic.Typed.Release.mkv', 4096, false, 'single', now(), now()
FROM generate_series(1, 100) i;
INSERT INTO torrent_contents (info_hash, content_type, created_at, updated_at)
SELECT info_hash, 'xxx', now(), now() FROM torrents;`)
	require.NoError(t, err)
	strong, media, err := classifier.CoreAdultServingEvidence()
	require.NoError(t, err)
	policy, err := serving.NewPolicy(serving.Config{ExcludeAdult: true}, strong, media)
	require.NoError(t, err)
	sql := dao.ToSQL(q.TorrentContent.UnderlyingDB().
		Where(policy.TorrentCondition("torrent_contents")).
		Where("torrent_contents.content_type = ?", "xxx"))
	var raw []byte
	require.NoError(t, pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql).Scan(&raw))
	var plans []map[string]any
	require.NoError(t, json.Unmarshal(raw, &plans))
	root := plans[0]["Plan"].(map[string]any)
	require.Zero(t, root["Actual Rows"])
	subplans := 0
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		if node["Parent Relationship"] == "SubPlan" || node["Parent Relationship"] == "InitPlan" {
			subplans++
			require.Zero(t, node["Actual Loops"], "typed adult rows must not scan other evidence")
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			visit(child.(map[string]any))
		}
	}
	visit(root)
	require.Positive(t, subplans, "the ordinary/unknown branch must retain the evidence guards")
}

func TestServingPostgresStrongEvidenceMatchesCoreBaseNameAndBasePath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, q := newServingPostgresDatabase(t, ctx)
	strong, media, err := classifier.CoreAdultServingEvidence()
	require.NoError(t, err)
	goPattern, err := keywords.NewRegexFromKeywords(strong...)
	require.NoError(t, err)
	p, err := serving.NewPolicy(serving.Config{ExcludeAdult: true}, strong, media)
	require.NoError(t, err)
	s, err := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return q, nil }), ServingPolicy: p}).ServingSearch.Get()
	require.NoError(t, err)
	cases := []struct {
		name   string
		paths  []string
		single bool
	}{
		{"Synthetic.Siterip.mkv", nil, true},
		{"Synthetic.SiteripReference.2031.mkv", nil, true},
		{"ÉsiteripΩ.2031.mkv", nil, true},
		{"١siterip١.mkv", nil, true},
		{"SyntheticMulti", []string{"Siterip/Film.mkv"}, false},
		{"SyntheticMulti", []string{"Film.mkv", "Sidecar.siterip"}, false},
		{"SyntheticMulti", []string{"Film.mkv", "Subtitle.ass"}, false},
		{"Synthetic.Siterip.zip", nil, true},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("context_%d", i), func(t *testing.T) {
			var ih protocol.ID
			ih[0] = byte(i + 40)
			status := "multi"
			if c.single {
				status = "single"
			}
			_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,$2,100,false,$3,$4,now(),now())`, ih.Bytes(), c.name, status, len(c.paths))
			require.NoError(t, err)
			tor := model.Torrent{Name: c.name, Size: 100, FilesStatus: model.FilesStatusMulti}
			if c.single {
				tor.FilesStatus = model.FilesStatusSingle
				tor.Extension = model.FileExtensionFromPath(c.name)
			}
			text := tor.BaseName()
			mediaBytes := 0
			if c.single {
				for _, ext := range media {
					if tor.Extension.Valid && tor.Extension.String == ext {
						mediaBytes = 100
					}
				}
			}
			for j, path := range c.paths {
				size := int64(1)
				if strings.HasSuffix(path, ".mkv") {
					size = 100
				}
				_, err = pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,$2,$3,$4,now(),now())`, ih.Bytes(), j, path, size)
				require.NoError(t, err)
				file := model.TorrentFile{Path: path, Extension: model.FileExtensionFromPath(path)}
				text += " " + file.BasePath()
				isMedia := false
				for _, ext := range media {
					if file.Extension.Valid && file.Extension.String == ext {
						isMedia = true
					}
				}
				if isMedia {
					mediaBytes += int(size)
				} else {
					mediaBytes -= int(size)
				}
			}
			wantHidden := mediaBytes > 0 && goPattern.MatchString(text)
			got, e := s.Torrents(ctx, query.Where(search.TorrentInfoHashCriteria(ih)))
			require.NoError(t, e)
			if wantHidden {
				require.Empty(t, got.Items)
			} else {
				require.Len(t, got.Items, 1)
			}
		})
	}
}

// The real API handlers and restore transaction run only against an invented,
// disposable schema. No worker, crawler, provider or model is started.
func TestServingNativePostgresAPIsAndActualRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, q := newServingPostgresDatabase(t, ctx)
	queryDAO := lazy.New(func() (*dao.Query, error) { return q, nil })
	lazyPool := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	strong, media, err := classifier.CoreAdultServingEvidence()
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO metadata_sources(key,name,created_at,updated_at) VALUES('synthetic','Synthetic metadata',now(),now());
INSERT INTO content(type,source,id,title,adult,created_at,updated_at) VALUES
 ('movie','synthetic','normal','NativeRegularFixture',NULL,now(),now()),
 ('movie','synthetic','adult','NativeAdultMetadataFixture',true,now(),now());`)
	require.NoError(t, err)
	names := []string{"NativeRegularFixture.mkv", "NativeTypedFixture.mkv", "NativeAdultMetadataFixture.mkv", "Native.Siterip.Fixture.mkv", "NativeRecrawledFixture.mkv"}
	hashes := make([]string, len(names))
	for i, name := range names {
		ih := make([]byte, 20)
		ih[0] = byte(i + 1)
		hashes[i] = hex.EncodeToString(ih)
		_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at)
VALUES($1,$2,4096,false,'multi',1,now(),now())`, ih, name)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at)
VALUES($1,0,$2,4096,now(),now())`, ih, name)
		require.NoError(t, err)
		ct := model.ContentTypeMovie
		id := "normal"
		if i == 1 {
			ct = model.ContentTypeXxx
			id = ""
		}
		if i == 2 {
			id = "adult"
		}
		tc := model.TorrentContent{InfoHash: model.Torrent{InfoHash: func() [20]byte { var x [20]byte; copy(x[:], ih); return x }()}.InfoHash, ContentType: model.NewNullContentType(ct), Size: 4096, PublishedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if id != "" {
			tc.ContentSource = model.NewNullString("synthetic")
			tc.ContentID = model.NewNullString(id)
		}
		tc.ID = tc.InferID()
		require.NoError(t, q.TorrentContent.WithContext(ctx).Create(&tc))
	}
	// Snapshot an ordinary torrent, then leave/repopulate its live rows exactly as
	// a recrawl can. The retained expired quarantine must still suppress serving.
	_, err = pool.Exec(ctx, `INSERT INTO junkpurge_quarantine(info_hash,torrent_name,verdict,confidence,quarantined_at,expired_at,torrent_snapshot,files_snapshot,sources_snapshot)
SELECT t.info_hash,t.name,'junk',0.99,now()-interval '45 days',now()-interval '1 day',to_jsonb(t),
(SELECT jsonb_agg(to_jsonb(f)) FROM torrent_files f WHERE f.info_hash=t.info_hash),'[]'::jsonb
FROM torrents t WHERE info_hash=decode($1,'hex')`, hashes[4])
	require.NoError(t, err)
	var originalData string
	require.NoError(t, pool.QueryRow(ctx, `SELECT md5(string_agg(to_jsonb(t)::text,'' ORDER BY info_hash)) FROM torrents t`).Scan(&originalData))
	router := func(enabled bool) *gin.Engine {
		p, e := serving.NewPolicy(serving.Config{ExcludeAdult: enabled}, strong, media)
		require.NoError(t, e)
		result := search.New(search.Params{Query: queryDAO, ServingPolicy: p})
		consumer, e := result.ServingSearch.Get()
		require.NoError(t, e)
		raw, e := result.Search.Get()
		require.NoError(t, e)
		rawRows, e := raw.Torrents(ctx)
		require.NoError(t, e)
		require.Len(t, rawRows.Items, 5, "processing/operator reads retain all raw rows")
		engine := gin.New()
		es := gql.NewExecutableSchema(gql.Config{Resolvers: &resolvers.Resolver{Search: consumer, Dao: q}})
		require.NoError(t, gqlhttp.New(gqlhttp.Params{Schema: lazy.New(func() (graphql.ExecutableSchema, error) { return es, nil })}).Option.Apply(engine))
		client := adapter.NewWithFilters(consumer, nil, false, nil, adapter.FreshnessConfig{})
		require.NoError(t, torznabhttp.New(lazy.New(func() (torznab.Client, error) { return client, nil }), torznab.NewDefaultConfig(), nil).Apply(engine))
		require.NoError(t, quarantinehttp.New(lazyPool, junkpurge.Config{}, nil, zap.NewNop().Sugar()).Apply(engine))
		return engine
	}
	graphqlRequest := func(engine *gin.Engine, query string) map[string]any {
		body, _ := json.Marshal(map[string]string{"query": query})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var result map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		require.NotContains(t, result, "errors")
		return result["data"].(map[string]any)
	}
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("adult_option_%t", enabled), func(t *testing.T) {
			engine := router(enabled)
			data := graphqlRequest(engine, `{torrentContent{search(input:{limit:20,totalCount:true,aggregationBudget:0}){totalCount items{infoHash torrent{name} content{adult}}}}}`)
			found := data["torrentContent"].(map[string]any)["search"].(map[string]any)
			want := 4
			if enabled {
				want = 1
			}
			require.Equal(t, float64(want), found["totalCount"])
			require.Len(t, found["items"], want)
			for _, item := range found["items"].([]any) {
				require.NotEqual(t, hashes[4], item.(map[string]any)["infoHash"])
			}
			for _, i := range []int{1, 2, 3, 4} {
				data = graphqlRequest(engine, fmt.Sprintf(`{torrent{files(input:{infoHashes:["%s"],totalCount:true}){totalCount items{path}}} torrentContent{search(input:{infoHashes:["%s"],totalCount:true,aggregationBudget:0}){totalCount items{infoHash}}}}`, hashes[i], hashes[i]))
				wantVisible := !enabled && i != 4
				n := float64(0)
				if wantVisible {
					n = 1
				}
				require.Equal(t, n, data["torrent"].(map[string]any)["files"].(map[string]any)["totalCount"])
				require.Equal(t, n, data["torrentContent"].(map[string]any)["search"].(map[string]any)["totalCount"])
			}
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/torznab/api?t=search&cat=6000", nil).WithContext(ctx))
			require.Equal(t, http.StatusOK, w.Code)
			if enabled {
				require.NotContains(t, w.Body.String(), "<item>")
			} else {
				require.Contains(t, w.Body.String(), "NativeTypedFixture")
			}
		})
	}
	var afterReads string
	require.NoError(t, pool.QueryRow(ctx, `SELECT md5(string_agg(to_jsonb(t)::text,'' ORDER BY info_hash)) FROM torrents t`).Scan(&afterReads))
	require.Equal(t, originalData, afterReads)
	engine := router(true)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/quarantine/"+hashes[4]+"/restore", nil).WithContext(ctx))
	require.Equal(t, http.StatusOK, w.Code)
	var quarantines, queued int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM junkpurge_quarantine`).Scan(&quarantines))
	require.Zero(t, quarantines)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM queue_jobs WHERE queue='process_torrent'`).Scan(&queued))
	require.Equal(t, 1, queued)
	data := graphqlRequest(engine, fmt.Sprintf(`{torrentContent{search(input:{infoHashes:["%s"],totalCount:true,aggregationBudget:0}){totalCount items{infoHash torrent{name}}}}}`, hashes[4]))
	require.Equal(t, float64(1), data["torrentContent"].(map[string]any)["search"].(map[string]any)["totalCount"])
	require.True(t, strings.HasPrefix(data["torrentContent"].(map[string]any)["search"].(map[string]any)["items"].([]any)[0].(map[string]any)["torrent"].(map[string]any)["name"].(string), "NativeRecrawledFixture"))
}
