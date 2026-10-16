package contentfilter

import (
	"context"
	"fmt"
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
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
)

type controlledPublicPrivacy struct{}

func (controlledPublicPrivacy) IsPrivateInfoHash(context.Context, []byte) (bool, error) {
	return false, nil
}

func controlledFilterPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("filter_dispatch_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() {
		db.Close()
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		admin.Close()
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	return pool
}

func TestControlledLanguageReviewReplaysAndHoldsAfterBodyRetention(t *testing.T) {
	pool := controlledFilterPool(t)
	ctx := context.Background()
	hash := make([]byte, 20)
	hash[0] = 37
	_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)VALUES($1,'Pelicula.2026',4096,false,'single',now(),now())`, hash)
	require.NoError(t, err)
	pg := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	store := llmcapture.NewPostgresStore(pg)
	ccfg := llmcapture.NewDefaultConfig()
	ccfg.Enabled = true
	capture, err := llmcapture.NewRecorder(ccfg, controlledPublicPrivacy{}, store)
	require.NoError(t, err)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reviewNegativeResponse))
	}))
	defer server.Close()
	cfg := phase2Config()
	cfg.LLMApiStyle = apiStyleChat
	cfg.LLMBaseURL = server.URL
	cfg.LLMAction = LLMActionReview
	cfg.LLMEnforce = "true"
	cfg.LLMDailyBudget = 1
	cfg.LLMMonthlyBudget = 1
	newFilter := func() *Filter {
		return NewWithLLMAdmission(cfg, NewOpenAIClientWithPolicy("synthetic-test", cfg.LLMModel, server.URL, apiStyleChat, cfg.LLMPromptVersion, "", cfg.LLMMaxOutputTokens, time.Second), LLMCallbacks{}, Admission{Budget: reviewBudgetProbe{allowed: false}, Capture: capture, Dispatch: llmcapture.NewPostgresDispatchController(store, true)})
	}
	in := Input{Title: "Pelicula.2026"}
	source := AuditSource{InfoHash: hash, GroupKey: []byte(EvaluationGroupKey(in.Title))}
	first, err := newFilter().DecideAudited(ctx, in, source)
	require.NoError(t, err)
	require.True(t, first.Allow)
	require.True(t, first.Review)
	require.False(t, first.WouldDrop)
	replayed, err := newFilter().DecideAudited(ctx, in, source)
	require.NoError(t, err)
	require.Equal(t, first, replayed)
	require.Equal(t, int32(1), calls.Load())
	var used int
	require.NoError(t, pool.QueryRow(ctx, `SELECT daily_calls FROM llm_request_budgets WHERE scope='contentfilter'`).Scan(&used))
	require.Equal(t, 1, used)
	_, err = pool.Exec(ctx, `INSERT INTO label_evidence(info_hash,source,category,source_kind,source_instance,source_object_id,observed_at,strength)VALUES($1,'qbittorrent','private','test','test','test',now(),1)`, hash)
	require.NoError(t, err)
	_, err = newFilter().DecideAudited(ctx, in, source)
	require.Error(t, err)
	require.Equal(t, int32(1), calls.Load())
	_, err = pool.Exec(ctx, `DELETE FROM label_evidence WHERE info_hash=$1`, hash)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE llm_evaluation_captures SET captured_at=now()-interval '2 days',expires_at=now()-interval '1 day'`)
	require.NoError(t, err)
	_, err = store.DeleteExpired(ctx)
	require.NoError(t, err)
	_, err = newFilter().DecideAudited(ctx, in, source)
	require.Error(t, err, "expired body cannot authorize another HTTP request")
	require.Equal(t, int32(1), calls.Load())
	require.NoError(t, pool.QueryRow(ctx, `SELECT daily_calls FROM llm_request_budgets WHERE scope='contentfilter'`).Scan(&used))
	require.Equal(t, 1, used)
}
