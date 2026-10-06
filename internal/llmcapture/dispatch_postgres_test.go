package llmcapture

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/lazy"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
)

func dispatchFixture(t *testing.T) (context.Context, *pgxpool.Pool, *PostgresStore, *Recorder, *PostgresDispatchController, Request, DispatchRequest) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("dispatch_fixture_%d", time.Now().UnixNano())
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
	store := NewPostgresStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	rcfg := NewDefaultConfig()
	rcfg.Enabled = true
	r, err := NewRecorder(rcfg, &privacyStub{}, store)
	require.NoError(t, err)
	req := validRequest()
	req.Task, req.CandidateSource = TaskMatcherEmbedding, CandidateSourceLocal
	_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,created_at,updated_at,files_status)
VALUES ($1,'SyntheticDispatch',4096,false,now(),now(),'single')`, req.InfoHash)
	require.NoError(t, err)
	outcome, err := r.Capture(ctx, req)
	require.NoError(t, err)
	require.Equal(t, OutcomeRecorded, outcome)
	key, err := KeyForRequest(req)
	require.NoError(t, err)
	return ctx, pool, store, r, NewPostgresDispatchController(store, true), req,
		DispatchRequest{CaptureKey: key, Task: req.Task, CandidateSource: req.CandidateSource,
			InfoHash: req.InfoHash, FreshCapture: true}
}

func TestDispatchControllerPostgres(t *testing.T) {
	t.Run("budget deny is durable predispatch and resumes after rollover", func(t *testing.T) {
		ctx, pool, _, recorder, controller, req, binding := dispatchFixture(t)
		_, err := pool.Exec(ctx, `INSERT INTO llm_request_budgets(scope,month_start,day_start,daily_calls,monthly_calls)
VALUES ('matcher',date_trunc('month',now() AT TIME ZONE 'UTC')::date,(now() AT TIME ZONE 'UTC')::date,15,75)`)
		require.NoError(t, err)
		lease, outcome, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, DispatchPrepared, outcome)
		allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
		require.False(t, allowed)
		var deferred *DispatchDeferredError
		require.ErrorAs(t, err, &deferred)
		require.Equal(t, "daily_budget", deferred.Reason)
		_, err = recorder.Capture(ctx, req)
		require.NoError(t, err)
		binding.FreshCapture = false
		_, _, err = controller.Prepare(ctx, binding)
		require.ErrorAs(t, err, &deferred)
		// A fixture advances the stored day marker, not a production allowance.
		// The normal reserve SQL remains the code that performs UTC rollover.
		_, err = pool.Exec(ctx, `UPDATE llm_request_budgets SET day_start=day_start-1;
UPDATE llm_capture_dispatch_attempts SET retry_after=clock_timestamp()-interval '1 second'`)
		require.NoError(t, err)
		lease, outcome, err = controller.Prepare(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, DispatchPrepared, outcome)
		allowed, err = controller.Reserve(ctx, lease, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		require.NoError(t, controller.BeginDispatch(ctx, lease))
		receipt, err := recorder.RecordHTTPResult(ctx, binding.CaptureKey, HTTPResult{Body: []byte(`{"data":[]}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		require.NoError(t, controller.ObserveResult(ctx, lease, receipt))
		_, outcome, err = controller.Prepare(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, DispatchReplay, outcome)
		replay, err := controller.Replay(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, []byte(`{"data":[]}`), replay.Result.Body)
		require.True(t, replay.Receipt.FromCache)
		var daily, monthly int
		require.NoError(t, pool.QueryRow(ctx, `SELECT daily_calls,monthly_calls FROM llm_request_budgets WHERE scope='matcher'`).Scan(&daily, &monthly))
		require.Equal(t, 1, daily)
		require.Equal(t, 76, monthly)
	})

	t.Run("legacy pending is unknown while legacy complete can replay", func(t *testing.T) {
		ctx, _, _, recorder, controller, _, binding := dispatchFixture(t)
		binding.FreshCapture = false
		_, outcome, err := controller.Prepare(ctx, binding)
		require.ErrorIs(t, err, ErrDispatchUnknown)
		require.Equal(t, DispatchUnknown, outcome)
		_, err = recorder.RecordHTTPResult(ctx, binding.CaptureKey, HTTPResult{Body: []byte(`{"first":true}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		_, outcome, err = controller.Prepare(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, DispatchReplay, outcome, "a retained exact first receipt reconciles the unknown hold without another HTTP call")
	})

	t.Run("intent survives capture TTL and cannot be reset", func(t *testing.T) {
		ctx, pool, store, recorder, controller, req, binding := dispatchFixture(t)
		lease, _, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		require.NoError(t, controller.BeginDispatch(ctx, lease))
		require.ErrorIs(t, controller.DeferNoDispatch(ctx, lease, "canceled", time.Now()), ErrDispatchLease)
		_, err = pool.Exec(ctx, `UPDATE llm_capture_dispatch_attempts SET lease_until=clock_timestamp()-interval '1 second';
UPDATE llm_evaluation_captures SET captured_at=clock_timestamp()-interval '2 minutes',expires_at=clock_timestamp()-interval '1 second';
UPDATE llm_evaluation_capture_admissions SET expires_at=clock_timestamp()-interval '1 second'`)
		require.NoError(t, err)
		removed, err := store.DeleteExpired(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, removed)
		outcome, err := recorder.Capture(ctx, req)
		require.NoError(t, err)
		require.Equal(t, OutcomeRecorded, outcome)
		_, _, err = controller.Prepare(ctx, binding)
		require.ErrorIs(t, err, ErrDispatchUnknown, "fresh raw capture after TTL is not proof the older request never dispatched")
		var calls int
		require.NoError(t, pool.QueryRow(ctx, `SELECT daily_calls FROM llm_request_budgets WHERE scope='matcher'`).Scan(&calls))
		require.Equal(t, 1, calls, "no automatic refund or second reservation")
	})

	t.Run("one owner across concurrent processes and stale owner fences", func(t *testing.T) {
		ctx, pool, _, _, controller, _, binding := dispatchFixture(t)
		var wg sync.WaitGroup
		leases := make(chan DispatchLease, 12)
		errs := make(chan error, 12)
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lease, outcome, err := controller.Prepare(ctx, binding)
				if outcome == DispatchPrepared && err == nil {
					leases <- lease
				} else if !errorsIsDispatchBusy(err) {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(leases)
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		require.Len(t, leases, 1)
		old := <-leases
		_, err := pool.Exec(ctx, `UPDATE llm_capture_dispatch_attempts SET lease_until=clock_timestamp()-interval '1 second'`)
		require.NoError(t, err)
		binding.FreshCapture = false
		fresh, _, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		require.Greater(t, fresh.Generation, old.Generation)
		_, err = controller.Reserve(ctx, old, "matcher", 15, 450)
		require.ErrorIs(t, err, ErrDispatchLease)
	})

	t.Run("privacy changes after reservation prevent the intent", func(t *testing.T) {
		ctx, pool, _, _, controller, req, binding := dispatchFixture(t)
		lease, _, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		_, err = pool.Exec(ctx, `UPDATE torrents SET private=true WHERE info_hash=$1`, req.InfoHash)
		require.NoError(t, err)
		require.ErrorIs(t, controller.BeginDispatch(ctx, lease), ErrDispatchLease)
		var state string
		require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_capture_dispatch_attempts`).Scan(&state))
		require.Equal(t, "admitted", state)
	})
}

func errorsIsDispatchBusy(err error) bool { return err == ErrDispatchBusy }
