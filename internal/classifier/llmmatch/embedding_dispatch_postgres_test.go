package llmmatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/spencercnorton/bitagent/internal/model"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
)

func embeddingDispatchFixture(t *testing.T) (context.Context, *pgxpool.Pool, *llmcapture.Recorder, *llmcapture.PostgresDispatchController, model.Torrent, CallBudget) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	extensionTx, err := admin.Begin(ctx)
	require.NoError(t, err)
	_, err = extensionTx.Exec(ctx, `SELECT pg_advisory_xact_lock(718435); CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public; CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, extensionTx.Commit(ctx))
	schema := fmt.Sprintf("embedding_dispatch_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() {
		_ = db.Close()
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	lazyPool := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	store := llmcapture.NewPostgresStore(lazyPool)
	captureCfg := llmcapture.NewDefaultConfig()
	captureCfg.Enabled = true
	recorder, err := llmcapture.NewRecorder(captureCfg, &privacyProbe{}, store)
	require.NoError(t, err)
	torrent := mediaTorrent("Synthetic.Film.2020.mkv")
	_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,created_at,updated_at,files_status)
VALUES ($1,$2,4096,false,now(),now(),'single')`, torrent.InfoHash.Bytes(), torrent.Name)
	require.NoError(t, err)
	return ctx, pool, recorder, llmcapture.NewPostgresDispatchController(store, true), torrent, NewPostgresCallBudget(lazyPool)
}

func TestEmbeddingDeferredDispatchPostgres(t *testing.T) {
	t.Run("same request after denial renews once then replays across restart", func(t *testing.T) {
		ctx, pool, recorder, control, torrent, budget := embeddingDispatchFixture(t)
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.WriteString(w, embeddingTestResponse)
		}))
		defer server.Close()
		newClient := func() *Client {
			client := embeddingTestClient(server.URL, recorder, budget).WithDispatchControl(control)
			client.cfg.DailyCallLimit, client.cfg.MonthlyCallLimit = 15, 450
			return client
		}
		_, err := pool.Exec(ctx, `INSERT INTO llm_request_budgets(scope,month_start,day_start,daily_calls,monthly_calls)
VALUES ('matcher',date_trunc('month',now() AT TIME ZONE 'UTC')::date,(now() AT TIME ZONE 'UTC')::date,15,75)`)
		require.NoError(t, err)
		ext := Extraction{Title: "Synthetic Film", Type: "movie", Year: 2020}
		request := func(client *Client) ([]Candidate, *embeddingShortlistAudit, *llmcapture.ResultTrace, error) {
			withTrace, trace := llmcapture.WithResultTrace(ctx)
			shortlist, audit, err := client.embeddingShortlist(withTrace, torrent, ext, embeddingTestCandidates(), llmcapture.CandidateSourceLocal)
			return shortlist, audit, trace, err
		}
		full, audit, _, err := request(newClient())
		require.NoError(t, err)
		require.Len(t, full, 3, "optional denial preserves full-candidate fallback")
		require.Equal(t, "fallback", audit.Outcome)
		require.Zero(t, calls.Load())
		full, _, _, err = request(newClient())
		require.NoError(t, err)
		require.Len(t, full, 3)
		_, err = pool.Exec(ctx, `UPDATE llm_request_budgets SET day_start=day_start-1;
UPDATE llm_capture_dispatch_attempts SET retry_after=clock_timestamp()-interval '1 second'`)
		require.NoError(t, err)
		shortlist, audit, _, err := request(newClient())
		require.NoError(t, err)
		require.Len(t, shortlist, 2)
		require.Equal(t, "shortlisted", audit.Outcome)
		require.EqualValues(t, 1, calls.Load())
		shortlist, audit, trace, err := request(newClient())
		require.NoError(t, err)
		require.Len(t, shortlist, 2)
		require.True(t, audit.Receipt.FromCache)
		receipt, ok := trace.Result(llmcapture.TaskMatcherEmbedding, llmcapture.CandidateSourceLocal)
		require.True(t, ok)
		require.True(t, receipt.FromCache, "original response authority is not a new observation")
		require.EqualValues(t, 1, calls.Load())
		var daily, monthly int
		require.NoError(t, pool.QueryRow(ctx, `SELECT daily_calls,monthly_calls FROM llm_request_budgets WHERE scope='matcher'`).Scan(&daily, &monthly))
		require.Equal(t, 1, daily)
		require.Equal(t, 76, monthly)
	})

	t.Run("legacy duplicate without dispatch proof remains unavailable", func(t *testing.T) {
		ctx, pool, recorder, control, torrent, budget := embeddingDispatchFixture(t)
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
		defer server.Close()
		_, err := pool.Exec(ctx, `INSERT INTO llm_request_budgets(scope,month_start,day_start,daily_calls,monthly_calls)
VALUES ('matcher',date_trunc('month',now() AT TIME ZONE 'UTC')::date,(now() AT TIME ZONE 'UTC')::date,15,75)`)
		require.NoError(t, err)
		ext := Extraction{Title: "Synthetic Film", Type: "movie", Year: 2020}
		plain := embeddingTestClient(server.URL, recorder, budget)
		plain.cfg.DailyCallLimit, plain.cfg.MonthlyCallLimit = 15, 450
		withTrace, _ := llmcapture.WithResultTrace(ctx)
		_, _, err = plain.embeddingShortlist(withTrace, torrent, ext, embeddingTestCandidates(), llmcapture.CandidateSourceLocal)
		require.NoError(t, err, "legacy denial creates pending capture without a dispatch fence")
		controlled := embeddingTestClient(server.URL, recorder, budget).WithDispatchControl(control)
		controlled.cfg.DailyCallLimit, controlled.cfg.MonthlyCallLimit = 15, 450
		withTrace, _ = llmcapture.WithResultTrace(ctx)
		_, _, err = controlled.embeddingShortlist(withTrace, torrent, ext, embeddingTestCandidates(), llmcapture.CandidateSourceLocal)
		require.ErrorIs(t, err, llmcapture.ErrCaptureUnavailable)
		require.Zero(t, calls.Load())
	})
}
