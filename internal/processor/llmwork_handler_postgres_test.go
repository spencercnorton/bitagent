package processor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func deferredTypeHarness(t *testing.T, beforeResponse func(*pgxpool.Pool, protocol.ID)) (*DeferredApplyHandler, *llmwork.Store, *llmwork.Lease, *llmstage.Stage, *pgxpool.Pool, model.Torrent, *atomic.Int32) {
	t.Helper()
	pool, backend := deferredApplyFixture(t)
	ctx := context.Background()
	var hash protocol.ID
	hash[0] = 23
	for _, q := range []string{`INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at)VALUES($1,'Amber.Signal.2026.1080p.mkv',734003200,false,'multi',1,now(),now())`, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,0,'Amber.Signal.2026.1080p.mkv',734003200,now(),now())`, `INSERT INTO torrent_contents(info_hash,size,is_anime,created_at,updated_at)VALUES($1,734003200,false,now(),now())`} {
		_, err := pool.Exec(ctx, q, hash.Bytes())
		require.NoError(t, err)
	}
	source := deferredTaskSource(t, backend, hash)
	pg := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	_, err := pg.Get()
	require.NoError(t, err)
	captureStore := llmcapture.NewPostgresStore(pg)
	captureCfg := llmcapture.NewDefaultConfig()
	captureCfg.Enabled = true
	recorder, err := llmcapture.NewRecorder(captureCfg, typeEnrichmentPublicPrivacy{}, captureStore)
	require.NoError(t, err)
	dispatch := llmcapture.NewPostgresDispatchController(captureStore, true)
	workCfg := llmwork.NewDefaultConfig()
	workCfg.Enabled = true
	workCfg.SpreadAdmission = false
	workCfg.TimeBuckets = 1
	work, err := llmwork.NewStore(workCfg, pg)
	require.NoError(t, err)
	work.SetDispatch(dispatch)
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if beforeResponse != nil {
			beforeResponse(pool, hash)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"category\":\"movie\",\"confidence\":0.99}"}}]}`))
	}))
	t.Cleanup(server.Close)
	stageCfg := llmstage.NewDefaultConfig()
	stageCfg.Enabled = true
	stageCfg.EnableLive = true
	stageCfg.LiveAllowedTypes = []string{"movie", "tv"}
	stageCfg.Endpoint = server.URL
	stageCfg.APIKey = "local-test"
	stageCfg.DailyCallLimit = 20
	stageCfg.MonthlyCallLimit = 20
	classifierCfg := classifier.NewDefaultConfig()
	inner := processRunnerStub{run: func(model.Torrent) (classification.Result, error) { panic("generic workflow must never execute") }}
	stage := llmstage.NewStage(stageCfg, inner, typeEnrichmentPublicPrivacy{}, llmstage.NewMetrics(), zap.NewNop().Sugar(), llmstage.Admission{Budget: llmmatch.NewPostgresTypeCallBudget(pg), Capture: recorder, Dispatch: dispatch})
	stage.SetWork(work, classifierCfg)
	p := llmstage.WorkPayload{Workflow: "default", Flags: classifier.Flags{"local_search_enabled": false}}
	body, err := json.Marshal(p)
	require.NoError(t, err)
	_, err = work.Enqueue(ctx, llmwork.Draft{Kind: llmwork.Type, InfoHash: hash.Bytes(), SourceDigest: llmwork.SourceDigest(source), PolicyDigest: llmwork.Digest(stage.WorkPolicy(p)), InputDigest: stage.WorkInputDigest(source), FamilyDigest: llmwork.Digest("synthetic"), Payload: body, DailyLimit: 20, MonthlyLimit: 20})
	require.NoError(t, err)
	lease, err := work.Claim(ctx, "fixture")
	require.NoError(t, err)
	require.NotNil(t, lease)
	handler := &DeferredApplyHandler{Pool: pg, Search: lazy.New(func() (search.Search, error) { return backend, nil }), Runner: lazy.New(func() (classifier.Runner, error) { return stage, nil }), Classifier: classifierCfg}
	return handler, work, lease, stage, pool, source, calls
}

func TestPostgresDeferredTypeApplyExactlyOnceAndPreserveAfterReceiptExpiry(t *testing.T) {
	h, work, lease, stage, pool, source, calls := deferredTypeHarness(t, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- h.Handle(llmwork.WithExecution(ctx, work, *lease), lease.Task) }()
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			require.True(t, errors.Is(err, llmwork.ErrLease) || errors.Is(err, llmwork.ErrObsolete) || errors.Is(err, llmcapture.ErrDispatchBusy) || errors.Is(err, llmcapture.ErrDispatchLease) || errors.Is(err, llmwork.ErrDeferred), "unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, success)
	require.EqualValues(t, 1, calls.Load())
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_work_applications`).Scan(&n))
	require.Equal(t, 1, n)
	require.Error(t, h.Handle(llmwork.WithExecution(ctx, work, *lease), lease.Task))
	_, err := pool.Exec(ctx, `DELETE FROM llm_evaluation_captures`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET payload='{}'::jsonb WHERE state='completed'`)
	require.NoError(t, err)
	result, err := stage.Run(ctx, "default", classifier.Flags{"local_search_enabled": false}, source)
	require.NoError(t, err)
	require.Equal(t, model.ContentTypeMovie, result.ContentType.ContentType)
	require.EqualValues(t, 1, calls.Load())
	sqlDB := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer sqlDB.Close()
	gdb, x := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, x)
	backend, x := h.Search.Get()
	require.NoError(t, x)
	ordinary := processor{dao: dao.Use(gdb), search: backend, runner: stage, defaultWorkflow: "default", logger: zap.NewNop().Sugar()}
	require.NoError(t, ordinary.Process(ctx, MessageParams{InfoHashes: []protocol.ID{source.InfoHash}, ClassifierFlags: classifier.Flags{"local_search_enabled": false}, SkipContentFilter: true}))
	require.EqualValues(t, 1, calls.Load(), "ordinary repeated processing retains the application")
	a, x := work.Preserve(ctx, llmwork.Type, source, stage.WorkPolicy(llmstage.WorkPayload{Workflow: "default", Flags: classifier.Flags{"local_search_enabled": false}}))
	require.NoError(t, x)
	require.NotNil(t, a)
	_, err = pool.Exec(ctx, `UPDATE torrent_files SET path='Changed.File.mkv' WHERE info_hash=$1`, source.InfoHash.Bytes())
	require.NoError(t, err)
	_, err = stage.Run(ctx, "default", classifier.Flags{"local_search_enabled": false}, source)
	require.ErrorIs(t, err, llmwork.ErrHeld)
	require.EqualValues(t, 1, calls.Load())
}

func TestPostgresDeferredApplyDeclinesChangedSourceAfterResponse(t *testing.T) {
	h, work, lease, _, pool, _, calls := deferredTypeHarness(t, func(pool *pgxpool.Pool, hash protocol.ID) {
		_, err := pool.Exec(context.Background(), `UPDATE torrent_files SET path='Source.Changed.mkv' WHERE info_hash=$1`, hash.Bytes())
		require.NoError(t, err)
	})
	err := h.Handle(llmwork.WithExecution(context.Background(), work, *lease), lease.Task)
	require.ErrorIs(t, err, llmwork.ErrObsolete)
	require.EqualValues(t, 1, calls.Load())
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM llm_work_applications`).Scan(&n))
	require.Zero(t, n)
	var typ model.NullContentType
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT content_type FROM torrent_contents`).Scan(&typ))
	require.False(t, typ.Valid)
}

func TestPostgresDeferredApplyDeclinesLatePrivacyHintAndTargetEdits(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE torrents SET private=true WHERE info_hash=$1`,
		`INSERT INTO label_evidence(info_hash,source,category,source_kind,source_instance,source_object_id,observed_at,strength)VALUES($1,'qbittorrent','private','test','test','test',now(),1)`,
		`INSERT INTO torrent_hints(info_hash,content_type,created_at,updated_at)VALUES($1,'movie',now(),now())`,
		`UPDATE torrent_contents SET content_type='tv_show' WHERE info_hash=$1`,
		`INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at)VALUES($1,1,'New.Evidence.mkv',1024,now(),now())`,
	} {
		t.Run(mutation, func(t *testing.T) {
			h, work, lease, _, pool, _, calls := deferredTypeHarness(t, func(pool *pgxpool.Pool, hash protocol.ID) {
				_, err := pool.Exec(context.Background(), mutation, hash.Bytes())
				require.NoError(t, err)
			})
			require.Error(t, h.Handle(llmwork.WithExecution(context.Background(), work, *lease), lease.Task))
			require.EqualValues(t, 1, calls.Load())
			var n int
			require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM llm_work_applications`).Scan(&n))
			require.Zero(t, n)
			var state string
			require.NoError(t, pool.QueryRow(context.Background(), `SELECT state FROM llm_work_tasks`).Scan(&state))
			require.Equal(t, "leased", state)
		})
	}
}

func TestPostgresDeferredApplyRollsBackFailedTargetWrite(t *testing.T) {
	h, work, lease, _, pool, _, calls := deferredTypeHarness(t, nil)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE FUNCTION reject_deferred_update() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'synthetic persistence failure';END$$; CREATE TRIGGER reject_deferred BEFORE UPDATE ON torrent_contents FOR EACH ROW EXECUTE FUNCTION reject_deferred_update()`)
	require.NoError(t, err)
	require.ErrorContains(t, h.Handle(llmwork.WithExecution(ctx, work, *lease), lease.Task), "synthetic persistence failure")
	require.EqualValues(t, 1, calls.Load())
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_work_applications`).Scan(&n))
	require.Zero(t, n)
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_work_tasks`).Scan(&state))
	require.Equal(t, "leased", state)
	_, err = pool.Exec(ctx, `DROP TRIGGER reject_deferred ON torrent_contents`)
	require.NoError(t, err)
	require.NoError(t, h.Handle(llmwork.WithExecution(ctx, work, *lease), lease.Task))
	require.EqualValues(t, 1, calls.Load(), "a known response replays after local rollback without redispatch")
}
