package llmcapture

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
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
	extensionTx, err := admin.Begin(ctx)
	require.NoError(t, err)
	_, err = extensionTx.Exec(ctx, `SELECT pg_advisory_xact_lock(718435); CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public; CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, extensionTx.Commit(ctx))
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

func dispatchCaseFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, req Request) *CaseFence {
	t.Helper()
	fence := &CaseFence{TaskKey: bytes.Repeat([]byte{31}, 32), LeaseOwner: "synthetic_owner", LeaseGeneration: 1,
		SourceDigest: bytes.Repeat([]byte{32}, 32), PolicyDigest: bytes.Repeat([]byte{33}, 32)}
	_, err := pool.Exec(ctx, `INSERT INTO llm_work_tasks
(task_key,kind,info_hash,source_digest,policy_digest,input_digest,family_digest,payload,priority,time_bucket,daily_limit,monthly_limit,state,lease_owner,lease_generation,lease_until,expires_at)
VALUES ($1,'matcher',$2,$3,$4,$3,$4,'{}'::jsonb,50,0,15,450,'leased',$5,1,clock_timestamp()+interval '1 minute',clock_timestamp()+interval '1 day')`,
		fence.TaskKey, req.InfoHash, fence.SourceDigest, fence.PolicyDigest, fence.LeaseOwner)
	require.NoError(t, err)
	fence.SourceRecheck = func(ctx context.Context, tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, `SELECT name FROM torrents WHERE info_hash=$1 FOR UPDATE`, req.InfoHash).Scan(&name); err != nil {
			return err
		}
		if name != "SyntheticDispatch" {
			return ErrDispatchLease
		}
		return nil
	}
	return fence
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

	for _, point := range []string{"admission", "commit"} {
		t.Run("budget and "+point+" failure roll back together", func(t *testing.T) {
			ctx, pool, _, _, controller, _, binding := dispatchFixture(t)
			lease, _, err := controller.Prepare(ctx, binding)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `CREATE FUNCTION synthetic_failure() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.state='admitted' THEN RAISE EXCEPTION 'synthetic admission failure'; END IF; RETURN NEW; END $$`)
			require.NoError(t, err)
			trigger := "CREATE TRIGGER fail_admission BEFORE UPDATE ON llm_capture_dispatch_attempts FOR EACH ROW EXECUTE FUNCTION synthetic_failure()"
			if point == "commit" {
				trigger = "CREATE CONSTRAINT TRIGGER fail_admission AFTER UPDATE ON llm_capture_dispatch_attempts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION synthetic_failure()"
			}
			_, err = pool.Exec(ctx, trigger)
			require.NoError(t, err)
			allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
			require.Error(t, err)
			require.False(t, allowed)
			var untouched bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM llm_request_budgets)
AND EXISTS(SELECT 1 FROM llm_capture_dispatch_attempts WHERE state='prepared')`).Scan(&untouched))
			require.True(t, untouched)
			_, err = pool.Exec(ctx, "DROP TRIGGER fail_admission ON llm_capture_dispatch_attempts")
			require.NoError(t, err)
			allowed, err = controller.Reserve(ctx, lease, "matcher", 15, 450)
			require.NoError(t, err)
			require.True(t, allowed)
		})
	}

	t.Run("joint source-case generation fence and late first-result recovery", func(t *testing.T) {
		ctx, pool, _, recorder, controller, req, binding := dispatchFixture(t)
		fence := dispatchCaseFixture(t, ctx, pool, req)
		binding.Case = fence
		lease, _, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		stripped := lease
		stripped.Case = nil
		require.ErrorIs(t, controller.BeginDispatch(ctx, stripped), ErrDispatchLease, "a linked source-case fence cannot be stripped")
		_, err = controller.Reserve(ctx, stripped, "matcher", 15, 450)
		require.ErrorIs(t, err, ErrDispatchLease)
		_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET lease_generation=2 WHERE task_key=$1`, fence.TaskKey)
		require.NoError(t, err)
		require.ErrorIs(t, controller.BeginDispatch(ctx, lease), ErrDispatchLease)
		_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET lease_generation=1 WHERE task_key=$1`, fence.TaskKey)
		require.NoError(t, err)
		require.NoError(t, controller.BeginDispatch(ctx, lease))
		// The process dies after immutable recording but before ObserveResult.
		_, err = recorder.RecordHTTPResult(ctx, binding.CaptureKey, HTTPResult{Body: []byte(`{"first":true}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		recovery, err := controller.TaskRecovery(ctx, fence.TaskKey)
		require.NoError(t, err)
		require.Len(t, recovery, 1)
		require.Equal(t, "result", recovery[0].State)
		require.True(t, recovery[0].Replayable)
		require.False(t, recovery[0].SafeToRetry)
	})

	t.Run("legacy completed replay links a case before crash recovery", func(t *testing.T) {
		ctx, pool, store, recorder, controller, req, binding := dispatchFixture(t)
		_, err := recorder.RecordHTTPResult(ctx, binding.CaptureKey, HTTPResult{Body: []byte(`{"first":true}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		binding.FreshCapture = false
		_, outcome, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, DispatchReplay, outcome)
		binding.Case = dispatchCaseFixture(t, ctx, pool, req)
		_, outcome, err = controller.Prepare(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, DispatchReplay, outcome)
		// A process restart sees the existing permanent request linked to its
		// source case, even if it died before recording case progress.
		restarted := NewPostgresDispatchController(store, true)
		recovery, err := restarted.TaskRecovery(ctx, binding.Case.TaskKey)
		require.NoError(t, err)
		require.Len(t, recovery, 1)
		require.True(t, recovery[0].Replayable)
		require.False(t, recovery[0].SafeToRetry)
		_, err = restarted.Replay(ctx, binding)
		require.NoError(t, err)
		binding.Case = nil
		_, err = restarted.Replay(ctx, binding)
		require.ErrorIs(t, err, ErrDispatchUnknown, "replay cannot strip its permanent source-case linkage")
	})

	t.Run("legacy unknown links its new case and remains held after restart", func(t *testing.T) {
		ctx, pool, store, _, controller, req, binding := dispatchFixture(t)
		binding.FreshCapture = false
		_, _, err := controller.Prepare(ctx, binding)
		require.ErrorIs(t, err, ErrDispatchUnknown)
		binding.Case = dispatchCaseFixture(t, ctx, pool, req)
		_, _, err = controller.Prepare(ctx, binding)
		require.ErrorIs(t, err, ErrDispatchUnknown)
		restarted := NewPostgresDispatchController(store, true)
		recovery, err := restarted.TaskRecovery(ctx, binding.Case.TaskKey)
		require.NoError(t, err)
		require.Len(t, recovery, 1, "unknown linkage must not appear as a case with no provider attempts")
		require.Equal(t, "unknown", recovery[0].State)
		require.False(t, recovery[0].Replayable)
		require.False(t, recovery[0].SafeToRetry)
	})

	t.Run("current source recheck and intent share locked evidence", func(t *testing.T) {
		ctx, pool, _, _, controller, req, binding := dispatchFixture(t)
		binding.Case = dispatchCaseFixture(t, ctx, pool, req)
		lease, _, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		_, err = pool.Exec(ctx, `UPDATE torrents SET name='ChangedSource' WHERE info_hash=$1`, req.InfoHash)
		require.NoError(t, err)
		require.ErrorIs(t, controller.BeginDispatch(ctx, lease), ErrDispatchLease, "a change after the outer callback prevents intent")
		_, err = pool.Exec(ctx, `UPDATE torrents SET name='SyntheticDispatch' WHERE info_hash=$1`, req.InfoHash)
		require.NoError(t, err)
		guard := lease.Case.SourceRecheck
		locked, release := make(chan struct{}), make(chan struct{})
		var once atomic.Bool
		lease.Case.SourceRecheck = func(ctx context.Context, tx pgx.Tx) error {
			if err := guard(ctx, tx); err != nil {
				return err
			}
			if once.CompareAndSwap(false, true) {
				close(locked)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		begun := make(chan error, 1)
		go func() { begun <- controller.BeginDispatch(ctx, lease) }()
		<-locked
		mutated := make(chan error, 1)
		go func() {
			_, err := pool.Exec(ctx, `UPDATE torrents SET name='ChangedSource' WHERE info_hash=$1`, req.InfoHash)
			mutated <- err
		}()
		select {
		case err := <-mutated:
			t.Fatalf("source mutation passed retained evidence lock: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		close(release)
		require.NoError(t, <-begun)
		require.NoError(t, <-mutated)
		var state string
		require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_capture_dispatch_attempts`).Scan(&state))
		require.Equal(t, "intent", state)
	})

	t.Run("monthly denial reports next month without changing limits", func(t *testing.T) {
		ctx, pool, _, _, controller, _, binding := dispatchFixture(t)
		_, err := pool.Exec(ctx, `INSERT INTO llm_request_budgets(scope,month_start,day_start,daily_calls,monthly_calls)
VALUES ('matcher',date_trunc('month',now() AT TIME ZONE 'UTC')::date,(now() AT TIME ZONE 'UTC')::date,0,450)`)
		require.NoError(t, err)
		lease, _, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
		require.False(t, allowed)
		var deferred *DispatchDeferredError
		require.ErrorAs(t, err, &deferred)
		require.Equal(t, "monthly_budget", deferred.Reason)
		require.Equal(t, 1, deferred.RetryAfterUTC.Day())
		var calls int
		require.NoError(t, pool.QueryRow(ctx, `SELECT monthly_calls FROM llm_request_budgets WHERE scope='matcher'`).Scan(&calls))
		require.Equal(t, 450, calls)
	})

	t.Run("completed digest without retained body stays held after TTL", func(t *testing.T) {
		ctx, pool, store, recorder, controller, req, binding := dispatchFixture(t)
		_, err := recorder.RecordHTTPResult(ctx, binding.CaptureKey, HTTPResult{Body: []byte(`{"first":true}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		binding.FreshCapture = false
		_, outcome, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, DispatchReplay, outcome)
		_, err = pool.Exec(ctx, `UPDATE llm_evaluation_captures SET captured_at=clock_timestamp()-interval '2 minutes',expires_at=clock_timestamp()-interval '1 second';
UPDATE llm_evaluation_capture_admissions SET expires_at=clock_timestamp()-interval '1 second'`)
		require.NoError(t, err)
		_, err = store.DeleteExpired(ctx)
		require.NoError(t, err)
		_, err = recorder.Capture(ctx, req)
		require.NoError(t, err)
		binding.FreshCapture = true
		_, _, err = controller.Prepare(ctx, binding)
		require.ErrorIs(t, err, ErrDispatchUnknown)
		// A newly inserted second body is not the original permanent authority.
		_, err = recorder.RecordHTTPResult(ctx, binding.CaptureKey, HTTPResult{Body: []byte(`{"second":true}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		_, _, err = controller.Prepare(ctx, binding)
		require.ErrorIs(t, err, ErrCaptureUnavailable)
	})

	t.Run("known cancellation before intent reuses its same-day slot", func(t *testing.T) {
		ctx, pool, _, _, controller, _, binding := dispatchFixture(t)
		lease, _, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		require.NoError(t, controller.DeferNoDispatch(ctx, lease, "canceled_before_intent", time.Now().UTC()))
		binding.FreshCapture = false
		fresh, _, err := controller.Prepare(ctx, binding)
		require.NoError(t, err)
		allowed, err = controller.Reserve(ctx, fresh, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		require.ErrorIs(t, controller.BeginDispatch(ctx, lease), ErrDispatchLease)
		require.NoError(t, controller.BeginDispatch(ctx, fresh))
		var calls int
		require.NoError(t, pool.QueryRow(ctx, `SELECT daily_calls FROM llm_request_budgets WHERE scope='matcher'`).Scan(&calls))
		require.Equal(t, 1, calls, "positive no-dispatch evidence does not buy or refund another same-day reservation")
	})
}

func errorsIsDispatchBusy(err error) bool { return err == ErrDispatchBusy }
