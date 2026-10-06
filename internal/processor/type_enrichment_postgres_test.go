package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	dbsearch "github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type typeEnrichmentPublicPrivacy struct{}

func (typeEnrichmentPublicPrivacy) IsPrivateInfoHash(context.Context, []byte) (bool, error) {
	return false, nil
}

// Exercise the actual default workflow, source-bound type recorder, local
// search, processor and persistence against an isolated fully migrated schema.
// The only HTTP endpoint is a local synthetic type provider.
func TestPostgresTypeLocalEnrichmentPersistenceRollbackAndRestart(t *testing.T) {
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("type_local_enrichment_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	pcfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	require.NoError(t, err)
	sqlDB := stdlib.OpenDB(*pcfg.ConnConfig)
	t.Cleanup(func() {
		_ = sqlDB.Close()
		pool.Close()
		cleanup, e := pgx.Connect(context.Background(), dsn)
		if e == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)
	dq := dao.Use(gdb)
	lazyDAO := lazy.New(func() (*dao.Query, error) { return dq, nil })
	lazyPool := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	search, err := dbsearch.New(dbsearch.Params{Query: lazyDAO}).Search.Get()
	require.NoError(t, err)
	captureCfg := llmcapture.NewDefaultConfig()
	captureCfg.Enabled = true
	recorder, err := llmcapture.NewRecorder(captureCfg, typeEnrichmentPublicPrivacy{}, llmcapture.NewPostgresStore(lazyPool))
	require.NoError(t, err)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		body, readErr := io.ReadAll(req.Body)
		if readErr != nil {
			t.Error(readErr)
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		category := "movie"
		if strings.Contains(string(body), "S02E04") {
			category = "tv"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{
			"content": fmt.Sprintf(`{"category":%q,"confidence":0.99}`, category),
		}}}})
	}))
	t.Cleanup(srv.Close)
	newProcessor := func() processor {
		cfg := classifier.NewDefaultConfig()
		factory := classifier.New(classifier.Params{
			Config: cfg,
			Search: lazy.New(func() (dbsearch.Search, error) { return search, nil }),
			// A remote metadata call would fail: this fixture has no TMDB client.
			TmdbClient: lazy.New(func() (tmdb.Client, error) { return nil, nil }),
		})
		compiler, e := factory.Compiler.Get()
		require.NoError(t, e)
		raw, e := os.ReadFile("../classifier/classifier.core.yml")
		require.NoError(t, e)
		var document map[string]any
		require.NoError(t, yaml.Unmarshal(raw, &document))
		encoded, e := json.Marshal(document)
		require.NoError(t, e)
		var source classifier.Source
		require.NoError(t, json.Unmarshal(encoded, &source))
		inner, e := compiler.Compile(source)
		require.NoError(t, e)
		typeCfg := llmstage.NewDefaultConfig()
		typeCfg.Enabled, typeCfg.EnableLive = true, true
		typeCfg.LiveAllowedTypes = []string{"movie", "tv"}
		typeCfg.Endpoint, typeCfg.APIKey = srv.URL, "local-test-key"
		typeCfg.DailyCallLimit, typeCfg.MonthlyCallLimit = 4, 4
		typeCfg.MaxConcurrentCalls = 2
		stage := llmstage.NewStage(typeCfg, inner, typeEnrichmentPublicPrivacy{}, llmstage.NewMetrics(), zap.NewNop().Sugar(), llmstage.Admission{
			Budget: llmmatch.NewPostgresTypeCallBudget(lazyPool), Capture: recorder,
		})
		return processor{defaultWorkflow: "default", search: search, runner: stage, dao: dq, logger: zap.NewNop().Sugar()}
	}
	flags := classifier.Flags{"apis_enabled": false, "tmdb_enabled": false, "llm_match_enabled": false}
	seed := func(prefix byte, name, ct, id, title string, year int, private bool) protocol.ID {
		var hash protocol.ID
		hash[0] = prefix
		_, e := pool.Exec(ctx, `INSERT INTO torrents (info_hash,name,size,private,files_status,files_count,created_at,updated_at)
VALUES ($1,$2,73400320,$3,'single',1,now(),now())`, hash.Bytes(), name, private)
		require.NoError(t, e)
		if id != "" {
			_, e = pool.Exec(ctx, `INSERT INTO content(type,source,id,title,release_year,release_date,tsv,created_at,updated_at)
VALUES ($1,'tmdb',$2,$3,$4,make_date($4,1,1),to_tsvector('simple',$3),now(),now())`, ct, id, title, year)
			require.NoError(t, e)
		}
		return hash
	}
	movie := seed(1, "Amber.Signal.2025.1080p.BluRay.x265-GROUP.mkv", "movie", "42", "Amber Signal", 2025, false)
	tv := seed(2, "Silver.Signal.S02E04.1080p.BluRay.x265-GROUP.mkv", "tv_show", "43", "Silver Signal", 2019, false)
	proc := newProcessor()
	params := MessageParams{InfoHashes: []protocol.ID{movie, tv}, ClassifierFlags: flags}
	require.NoError(t, proc.Process(ctx, params))
	assertStored := func(hash protocol.ID, ct, id string) {
		var storedType, source, storedID, resolution, videoSource, codec, group string
		require.NoError(t, pool.QueryRow(ctx, `SELECT content_type,content_source,content_id,video_resolution,video_source,video_codec,release_group
FROM torrent_contents WHERE info_hash=$1`, hash.Bytes()).Scan(&storedType, &source, &storedID, &resolution, &videoSource, &codec, &group))
		require.Equal(t, ct, storedType)
		require.Equal(t, "tmdb", source)
		require.Equal(t, id, storedID)
		require.Equal(t, "V1080p", resolution)
		require.Equal(t, "BluRay", videoSource)
		require.Equal(t, "x265", codec)
		require.Equal(t, "GROUP", group)
		var tags int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrent_tags WHERE info_hash=$1 AND name='type-local-enriched'`, hash.Bytes()).Scan(&tags))
		require.Equal(t, 1, tags)
	}
	assertStored(movie, "movie", "42")
	assertStored(tv, "tv_show", "43")
	var episodes []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT episodes FROM torrent_contents WHERE info_hash=$1", tv.Bytes()).Scan(&episodes))
	require.JSONEq(t, `{"2":{"4":{}}}`, string(episodes))
	require.EqualValues(t, 2, calls.Load())
	var boundDecisions int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_evaluation_capture_results r
JOIN llm_evaluation_capture_admissions a USING(capture_key)
JOIN llm_evaluation_captures c USING(capture_key)
WHERE c.task='classifier_type' AND r.http_status=200 AND r.decision->>'outcome'='classified'
AND a.info_hash IN ($1,$2)`, movie.Bytes(), tv.Bytes()).Scan(&boundDecisions))
	require.Equal(t, 2, boundDecisions)
	// A new stage/runner models a restart: the persisted identities avoid new
	// type inference without requiring a warm in-memory model cache.
	restarted := newProcessor()
	require.NoError(t, restarted.Process(ctx, params))
	assertStored(movie, "movie", "42")
	assertStored(tv, "tv_show", "43")
	require.EqualValues(t, 2, calls.Load())
	private := seed(3, "Private.Signal.2025.1080p.mkv", "", "", "", 0, true)
	require.NoError(t, proc.Process(ctx, MessageParams{InfoHashes: []protocol.ID{private}, ClassifierFlags: flags}))
	var privateIDs int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM torrent_contents WHERE info_hash=$1 AND content_id IS NOT NULL", private.Bytes()).Scan(&privateIDs))
	require.Zero(t, privateIDs)
	require.EqualValues(t, 2, calls.Load())

	failed := seed(4, "Golden.Signal.2025.1080p.BluRay.x265-GROUP.mkv", "movie", "44", "Golden Signal", 2025, false)
	_, err = pool.Exec(ctx, `CREATE FUNCTION fail_type_enrichment_tag() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.info_hash=decode('0400000000000000000000000000000000000000','hex') THEN RAISE EXCEPTION 'synthetic tag failure'; END IF; RETURN NEW; END $$;
CREATE TRIGGER fail_type_enrichment_tag BEFORE INSERT ON torrent_tags FOR EACH ROW EXECUTE FUNCTION fail_type_enrichment_tag();`)
	require.NoError(t, err)
	failedParams := MessageParams{InfoHashes: []protocol.ID{failed}, ClassifierFlags: flags}
	require.Error(t, proc.Process(ctx, failedParams))
	var retained int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM torrent_contents WHERE info_hash=$1", failed.Bytes()).Scan(&retained))
	require.Zero(t, retained, "a failed tag write must roll back the identity and attributes")
	require.EqualValues(t, 3, calls.Load())
	_, err = pool.Exec(ctx, "DROP TRIGGER fail_type_enrichment_tag ON torrent_tags")
	require.NoError(t, err)
	require.NoError(t, proc.Process(ctx, failedParams))
	assertStored(failed, "movie", "44")
	require.EqualValues(t, 3, calls.Load(), "warm retry reuses the exact audited type response")
	var used int
	require.NoError(t, pool.QueryRow(ctx, "SELECT daily_calls FROM llm_request_budgets WHERE scope='classifier_type'").Scan(&used))
	require.Equal(t, 3, used)
}
