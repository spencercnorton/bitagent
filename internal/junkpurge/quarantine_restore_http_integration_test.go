package junkpurge_test

import (
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

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/junkpurge"
	"github.com/spencercnorton/bitagent/internal/junkpurge/quarantinehttp"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"github.com/spencercnorton/bitagent/internal/torznab/adapter"
	torznabhttp "github.com/spencercnorton/bitagent/internal/torznab/httpserver"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// The classifier is deliberately local and deterministic. The actual processor,
// persistence, source hydration and default Torznab HTTP path remain exercised.
type restoreFixtureRunner struct{ classifier.Runner }

func (restoreFixtureRunner) Run(context.Context, string, classifier.Flags, model.Torrent) (classification.Result, error) {
	return classification.Result{}, nil
}

func TestQuarantineHTTPRestorePreservesSourcesAndServing(t *testing.T) {
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to run PostgreSQL integration coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	schema := fmt.Sprintf("quarantine_restore_http_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
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
	vs := verdicts.NewStore(lazyPool)
	s, err := search.New(search.Params{Query: lazyDAO}).Search.Get()
	require.NoError(t, err)
	proc, err := processor.New(processor.Params{
		ClassifierConfig: classifier.NewDefaultConfig(),
		Dao:              lazyDAO,
		Search:           lazy.New(func() (search.Search, error) { return s, nil }),
		Workflow:         lazy.New(func() (classifier.Runner, error) { return restoreFixtureRunner{}, nil }),
		BlockingManager:  lazy.New(func() (blocking.Manager, error) { return nil, nil }),
		Logger:           zap.NewNop().Sugar(),
	}).Processor.Get()
	require.NoError(t, err)
	client := adapter.NewWithFilters(s, nil, false, nil, adapter.FreshnessConfigFromTorznab(torznab.NewDefaultConfig())).WithVerdicts(vs, true, nil)
	e := gin.New()
	require.NoError(t, quarantinehttp.New(lazyPool, junkpurge.Config{}, vs, zap.NewNop().Sugar()).Apply(e))
	require.NoError(t, torznabhttp.New(lazy.New(func() (torznab.Client, error) { return client, nil }), torznab.NewDefaultConfig(), nil).Apply(e))
	request := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest(method, path, nil).WithContext(ctx))
		return w
	}

	// Capture actual native rows, then delete only this invented torrent. The
	// worker's same-transaction snapshot capture is covered by the lifecycle test.
	seedSnapshot := func(t *testing.T, prefix byte, legacy bool, trackerZero bool) (string, string) {
		t.Helper()
		ih := make([]byte, 20)
		ih[0] = prefix
		hash := hex.EncodeToString(ih)
		source := fmt.Sprintf("synthetic_%d", prefix)
		_, err := pool.Exec(ctx, `INSERT INTO torrents (info_hash,name,size,private,files_status,files_count,created_at,updated_at)
VALUES ($1,'SyntheticRecovery.ENG.mkv',4096,false,'multi',1,now()-interval '60 days',now()-interval '40 days')`, ih)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrent_files (info_hash,"index",path,size,created_at,updated_at)
VALUES ($1,0,'SyntheticRecovery/SyntheticRecovery.ENG.mkv',4096,now()-interval '60 days',now()-interval '40 days')`, ih)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrent_sources(key,name,created_at,updated_at)
VALUES ($1,'Synthetic source',now()-interval '100 days',now()-interval '80 days')`, source)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrents_torrent_sources
(source,info_hash,import_id,seeders,leechers,published_at,created_at,updated_at)
VALUES ($1,$2,'synthetic-import',NULL,2,now()-interval '70 days',now()-interval '60 days',now()-interval '40 days')`, source, ih)
		require.NoError(t, err)
		if trackerZero {
			_, err = pool.Exec(ctx, `INSERT INTO torrents_torrent_sources (source,info_hash,seeders,leechers,created_at,updated_at)
VALUES ('tracker',$1,0,0,now()-interval '40 days',now()-interval '40 days')`, ih)
			require.NoError(t, err)
		}
		var original string
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(ts)::text FROM torrents_torrent_sources ts WHERE source=$1 AND info_hash=$2`, source, ih).Scan(&original))
		_, err = pool.Exec(ctx, `INSERT INTO junkpurge_quarantine
(info_hash,torrent_name,verdict,confidence,quarantined_at,expired_at,torrent_snapshot,files_snapshot,sources_snapshot)
SELECT t.info_hash,t.name,'junk',0.99,now()-interval '40 days',now()-interval '1 day',to_jsonb(t),
(SELECT jsonb_agg(to_jsonb(f)) FROM torrent_files f WHERE f.info_hash=t.info_hash),
CASE WHEN $2 THEN NULL ELSE (SELECT jsonb_agg(to_jsonb(ts)||jsonb_build_object('source_metadata',to_jsonb(s)))
FROM torrents_torrent_sources ts JOIN torrent_sources s ON s.key=ts.source WHERE ts.info_hash=t.info_hash) END
FROM torrents t WHERE t.info_hash=$1`, ih, legacy)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `DELETE FROM torrents WHERE info_hash=$1`, ih)
		require.NoError(t, err)
		require.NoError(t, vs.Record(ctx, verdicts.Event{InfoHash: ih, Verdict: verdicts.VerdictTombstoned, Mechanism: verdicts.MechanismJunkpurge}))
		return hash, original
	}
	processQueued := func(t *testing.T, hash string) {
		t.Helper()
		var raw []byte
		require.NoError(t, pool.QueryRow(ctx, `SELECT payload FROM queue_jobs WHERE queue='process_torrent' AND payload->'InfoHashes'=$1::jsonb`, `["`+hash+`"]`).Scan(&raw))
		var msg processor.MessageParams
		require.NoError(t, json.Unmarshal(raw, &msg))
		require.Equal(t, processor.ClassifyModeRematch, msg.ClassifyMode)
		require.NoError(t, proc.Process(ctx, msg))
		var indexed int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrent_contents WHERE info_hash=decode($1,'hex') AND tsv @@ plainto_tsquery('simple',$1)`, hash).Scan(&indexed))
		require.Equal(t, 1, indexed, "actual processor must regenerate searchable content")
	}
	assertRestored := func(t *testing.T, hash string) {
		t.Helper()
		var torrents, files, quarantine int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrents WHERE info_hash=decode($1,'hex')`, hash).Scan(&torrents))
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrent_files WHERE info_hash=decode($1,'hex')`, hash).Scan(&files))
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM junkpurge_quarantine WHERE info_hash=decode($1,'hex')`, hash).Scan(&quarantine))
		require.Equal(t, 1, torrents)
		require.Equal(t, 1, files)
		require.Zero(t, quarantine)
		var localUnknown bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT seeders IS NULL AND leechers IS NULL AND published_at IS NULL
AND import_id IS NULL AND updated_at >= now()-interval '1 minute'
FROM torrents_torrent_sources WHERE info_hash=decode($1,'hex') AND source='quarantine_restore'`, hash).Scan(&localUnknown))
		require.True(t, localUnknown, "local provenance must never invent swarm observations")
	}

	t.Run("expired authentic snapshot restores source metadata and default serving", func(t *testing.T) {
		hash, original := seedSnapshot(t, 1, false, false)
		// The source registry itself may have been removed after quarantine.
		var metadata string
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(s)::text FROM torrent_sources s WHERE key='synthetic_1'`).Scan(&metadata))
		_, err := pool.Exec(ctx, `DELETE FROM torrent_sources WHERE key='synthetic_1'`)
		require.NoError(t, err)
		w := request(http.MethodPost, "/api/quarantine/"+hash+"/restore")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assertRestored(t, hash)
		var restored, restoredMeta string
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(ts)::text FROM torrents_torrent_sources ts WHERE source='synthetic_1' AND info_hash=decode($1,'hex')`, hash).Scan(&restored))
		require.Equal(t, original, restored, "original seed/leech counts and publish/observation times must round-trip exactly")
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(s)::text FROM torrent_sources s WHERE key='synthetic_1'`).Scan(&restoredMeta))
		require.Equal(t, metadata, restoredMeta)
		processQueued(t, hash)
		w = request(http.MethodGet, "/torznab/?t=search&q="+hash)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "<guid>"+hash+"</guid>", "default freshness policy must allow a recent explicit restore")
		require.NotContains(t, w.Body.String(), `name="seeders"`, "unknown swarm counts remain unknown")
	})
	t.Run("legacy NULL snapshot remains restorable and visible", func(t *testing.T) {
		hash, _ := seedSnapshot(t, 2, true, false)
		w := request(http.MethodPost, "/api/quarantine/"+hash+"/restore")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assertRestored(t, hash)
		processQueued(t, hash)
		w = request(http.MethodGet, "/torznab/?t=search&q="+hash)
		require.Contains(t, w.Body.String(), "<guid>"+hash+"</guid>")
	})
	t.Run("known tracker zero remains excluded", func(t *testing.T) {
		hash, _ := seedSnapshot(t, 3, false, true)
		w := request(http.MethodPost, "/api/quarantine/"+hash+"/restore")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assertRestored(t, hash)
		processQueued(t, hash)
		w = request(http.MethodGet, "/torznab/?t=search&q="+hash)
		require.NotContains(t, w.Body.String(), "<guid>"+hash+"</guid>")
	})
	t.Run("queue failure rolls back restoration and allows actual retry", func(t *testing.T) {
		hash, _ := seedSnapshot(t, 4, false, false)
		_, err := pool.Exec(ctx, `CREATE FUNCTION refuse_restore_queue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic queue failure'; END $$`)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `CREATE TRIGGER refuse_restore_queue BEFORE INSERT ON queue_jobs FOR EACH ROW EXECUTE FUNCTION refuse_restore_queue()`)
		require.NoError(t, err)
		w := request(http.MethodPost, "/api/quarantine/"+hash+"/restore")
		require.Equal(t, http.StatusInternalServerError, w.Code)
		var remains bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=decode($1,'hex') AND sources_snapshot IS NOT NULL)
AND NOT EXISTS(SELECT 1 FROM torrents WHERE info_hash=decode($1,'hex'))
AND NOT EXISTS(SELECT 1 FROM torrent_files WHERE info_hash=decode($1,'hex'))
AND NOT EXISTS(SELECT 1 FROM torrents_torrent_sources WHERE info_hash=decode($1,'hex'))
AND NOT EXISTS(SELECT 1 FROM torrent_verdict_events WHERE info_hash=decode($1,'hex') AND verdict='restored')`, hash).Scan(&remains))
		require.True(t, remains, "failure must retain the whole recovery snapshot and emit no restore event")
		_, err = pool.Exec(ctx, `DROP TRIGGER refuse_restore_queue ON queue_jobs`)
		require.NoError(t, err)
		w = request(http.MethodPost, "/api/quarantine/"+hash+"/restore")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assertRestored(t, hash)
		processQueued(t, hash)
	})
	t.Run("malformed retained sources roll back every restore write", func(t *testing.T) {
		hash, _ := seedSnapshot(t, 6, false, false)
		_, err := pool.Exec(ctx, `UPDATE junkpurge_quarantine SET sources_snapshot='{}'::jsonb WHERE info_hash=decode($1,'hex')`, hash)
		require.NoError(t, err)
		w := request(http.MethodPost, "/api/quarantine/"+hash+"/restore")
		require.Equal(t, http.StatusInternalServerError, w.Code)
		var rolledBack bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=decode($1,'hex'))
AND NOT EXISTS(SELECT 1 FROM torrents WHERE info_hash=decode($1,'hex'))
AND NOT EXISTS(SELECT 1 FROM torrent_files WHERE info_hash=decode($1,'hex'))
AND NOT EXISTS(SELECT 1 FROM torrents_torrent_sources WHERE info_hash=decode($1,'hex'))
AND NOT EXISTS(SELECT 1 FROM queue_jobs WHERE payload->'InfoHashes'=$2::jsonb)
AND NOT EXISTS(SELECT 1 FROM torrent_verdict_events WHERE info_hash=decode($1,'hex') AND verdict='restored')`, hash, `["`+hash+`"]`).Scan(&rolledBack))
		require.True(t, rolledBack)
	})
	t.Run("newer reacquired source observations are never overwritten", func(t *testing.T) {
		hash, original := seedSnapshot(t, 7, false, false)
		_, err := pool.Exec(ctx, `INSERT INTO torrents (info_hash,name,size,private,files_status,files_count,created_at,updated_at)
SELECT info_hash,name,size,private,files_status,files_count,created_at,updated_at
FROM jsonb_populate_record(NULL::torrents,(SELECT torrent_snapshot FROM junkpurge_quarantine WHERE info_hash=decode($1,'hex')))`, hash)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrents_torrent_sources(source,info_hash,seeders,leechers,created_at,updated_at)
VALUES ('synthetic_7',decode($1,'hex'),9,4,now(),now())`, hash)
		require.NoError(t, err)
		var newer string
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(ts)::text FROM torrents_torrent_sources ts WHERE source='synthetic_7' AND info_hash=decode($1,'hex')`, hash).Scan(&newer))
		require.NotEqual(t, original, newer)
		w := request(http.MethodPost, "/api/quarantine/"+hash+"/restore")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assertRestored(t, hash)
		var retained string
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(ts)::text FROM torrents_torrent_sources ts WHERE source='synthetic_7' AND info_hash=decode($1,'hex')`, hash).Scan(&retained))
		require.Equal(t, newer, retained, "restore must not replace newer source observations with historical counts")
	})
	t.Run("captured empty source set remains usable", func(t *testing.T) {
		hash, _ := seedSnapshot(t, 8, false, false)
		_, err := pool.Exec(ctx, `UPDATE junkpurge_quarantine SET sources_snapshot='[]'::jsonb WHERE info_hash=decode($1,'hex')`, hash)
		require.NoError(t, err)
		w := request(http.MethodPost, "/api/quarantine/"+hash+"/restore")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assertRestored(t, hash)
		processQueued(t, hash)
		w = request(http.MethodGet, "/torznab/?t=search&q="+hash)
		require.Contains(t, w.Body.String(), "<guid>"+hash+"</guid>")
	})
	t.Run("Down preserves retained source snapshots", func(t *testing.T) {
		hash, _ := seedSnapshot(t, 5, false, false)
		migration, err := migrationssql.FS.ReadFile("00055_quarantine_source_snapshots.sql")
		require.NoError(t, err)
		down := strings.SplitN(string(migration), "-- +goose Down", 2)[1]
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, down)
		require.ErrorContains(t, err, "quarantine source snapshots must be retained")
		require.NoError(t, tx.Rollback(ctx))
		var retained bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT sources_snapshot IS NOT NULL FROM junkpurge_quarantine WHERE info_hash=decode($1,'hex')`, hash).Scan(&retained))
		require.True(t, retained)
	})
}
