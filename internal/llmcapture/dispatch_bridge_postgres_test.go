package llmcapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func bridgeCapture(t *testing.T, ctx context.Context, recorder *Recorder, req Request, build string) (Request, DispatchRequest) {
	t.Helper()
	req.BuildIdentity = build
	outcome, err := recorder.Capture(ctx, req)
	require.NoError(t, err)
	key, err := KeyForRequest(req)
	require.NoError(t, err)
	semantic, err := SemanticKeyForRequest(req)
	require.NoError(t, err)
	return req, DispatchRequest{CaptureKey: key, SemanticKey: semantic, Task: req.Task, CandidateSource: req.CandidateSource, InfoHash: req.InfoHash, FreshCapture: outcome == OutcomeRecorded}
}

func bridgeComplete(t *testing.T, ctx context.Context, recorder *Recorder, controller *PostgresDispatchController, binding DispatchRequest) ResultReceipt {
	t.Helper()
	lease, outcome, err := controller.Prepare(ctx, binding)
	require.NoError(t, err)
	require.Equal(t, DispatchPrepared, outcome)
	allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NoError(t, controller.BeginDispatch(ctx, lease))
	receipt, err := recorder.RecordHTTPResult(ctx, lease.CaptureKey, HTTPResult{Body: []byte(`{"first":true}`), StatusCode: 200, ErrorClass: "none"})
	require.NoError(t, err)
	require.NoError(t, controller.ObserveResult(ctx, lease, receipt))
	return receipt
}

func bridgeCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fences, results, calls int) {
	t.Helper()
	var actualFences, actualResults, actualCalls int
	require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM llm_capture_dispatch_attempts),
 (SELECT count(*) FROM llm_evaluation_capture_results),COALESCE((SELECT sum(daily_calls) FROM llm_request_budgets),0)`).Scan(&actualFences, &actualResults, &actualCalls))
	require.Equal(t, fences, actualFences)
	require.Equal(t, results, actualResults)
	require.Equal(t, calls, actualCalls)
}

func TestDispatchSemanticBridgePostgres(t *testing.T) {
	t.Run("build change replays original source and immutable decision authority", func(t *testing.T) {
		ctx, pool, _, recorder, controller, req, _ := dispatchFixture(t)
		req.Task, req.CandidateSource = TaskMatcherRerank, CandidateSourceLocal
		req, old := bridgeCapture(t, ctx, recorder, req, "synthetic-build-old")
		old.Case = dispatchCaseFixture(t, ctx, pool, req)
		original := bridgeComplete(t, ctx, recorder, controller, old)
		decision := MatchDecision{Outcome: "abstained", GateReason: "low_confidence", Confidence: .2, MinConfidence: .9}
		require.NoError(t, recorder.RecordMatchDecision(ctx, original, req.InfoHash, decision))
		_, current := bridgeCapture(t, ctx, recorder, req, "synthetic-build-new")
		current.Case = old.Case
		require.NotEqual(t, old.CaptureKey, current.CaptureKey)
		require.Equal(t, old.SemanticKey, current.SemanticKey)
		_, outcome, err := controller.Prepare(ctx, current)
		require.NoError(t, err)
		require.Equal(t, DispatchReplay, outcome)
		replay, err := controller.Replay(ctx, current)
		require.NoError(t, err)
		require.Equal(t, original.CaptureKey, replay.Receipt.CaptureKey)
		require.Equal(t, original.ResponseSHA256, replay.Receipt.ResponseSHA256)
		require.True(t, replay.Receipt.FromCache)
		require.True(t, replay.Receipt.FirstObservation)
		require.NoError(t, recorder.RecordMatchDecision(ctx, replay.Receipt, req.InfoHash, decision))
		changed := decision
		changed.GateReason = "changed"
		require.ErrorIs(t, recorder.RecordMatchDecision(ctx, replay.Receipt, req.InfoHash, changed), ErrCaptureUnavailable)
		var build string
		var newResult bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT build_identity FROM llm_evaluation_captures WHERE capture_key=$1`, replay.Receipt.CaptureKey).Scan(&build))
		require.Equal(t, "synthetic-build-old", build)
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM llm_evaluation_capture_results WHERE capture_key=$1)`, current.CaptureKey).Scan(&newResult))
		require.False(t, newResult, "replay cannot synthesize an HTTP observation in the new build cohort")
		bridgeCounts(t, ctx, pool, 1, 1, 1)
	})

	for _, kind := range []string{"intent", "transport", "missing_body", "expired_body"} {
		t.Run(kind+" holds across upgrade and capture retention", func(t *testing.T) {
			ctx, pool, store, recorder, controller, req, old := dispatchFixture(t)
			old.SemanticKey, _ = SemanticKeyForRequest(req)
			lease, _, err := controller.Prepare(ctx, old)
			require.NoError(t, err)
			allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
			require.NoError(t, err)
			require.True(t, allowed)
			require.NoError(t, controller.BeginDispatch(ctx, lease))
			if kind != "intent" {
				status, errorClass := 200, "none"
				if kind == "transport" {
					status, errorClass = 0, "transport"
				}
				receipt, err := recorder.RecordHTTPResult(ctx, old.CaptureKey, HTTPResult{Body: []byte(`{"first":true}`), StatusCode: status, ErrorClass: errorClass})
				require.NoError(t, err)
				require.NoError(t, controller.ObserveResult(ctx, lease, receipt))
			}
			_, err = pool.Exec(ctx, `UPDATE llm_capture_dispatch_attempts SET lease_until=clock_timestamp()-interval '1 second'`)
			require.NoError(t, err)
			if kind == "missing_body" {
				_, err = pool.Exec(ctx, `DELETE FROM llm_evaluation_capture_results`)
				require.NoError(t, err)
			}
			if kind == "expired_body" || kind == "intent" || kind == "transport" {
				_, err = pool.Exec(ctx, `UPDATE llm_evaluation_captures SET captured_at=clock_timestamp()-interval '2 minutes',expires_at=clock_timestamp()-interval '1 second';
UPDATE llm_evaluation_capture_admissions SET expires_at=clock_timestamp()-interval '1 second'`)
				require.NoError(t, err)
				_, err = store.DeleteExpired(ctx)
				require.NoError(t, err)
			}
			_, current := bridgeCapture(t, ctx, recorder, req, "synthetic-build-new")
			restarted := NewPostgresDispatchController(store, true)
			_, outcome, err := restarted.Prepare(ctx, current)
			require.ErrorIs(t, err, ErrDispatchUnknown)
			require.Equal(t, DispatchUnknown, outcome)
			_, err = restarted.Replay(ctx, current)
			require.ErrorIs(t, err, ErrDispatchUnknown)
			var active []byte
			var state string
			require.NoError(t, pool.QueryRow(ctx, `SELECT active_capture_key,state FROM llm_capture_dispatch_attempts`).Scan(&active, &state))
			require.Equal(t, old.CaptureKey, active)
			require.Equal(t, "unknown", state)
			bridgeCounts(t, ctx, pool, 1, 0, 1)
		})
	}

	t.Run("identical replacement body after retention cannot renew first receipt", func(t *testing.T) {
		ctx, pool, store, recorder, controller, req, old := dispatchFixture(t)
		old.Case = dispatchCaseFixture(t, ctx, pool, req)
		original := bridgeComplete(t, ctx, recorder, controller, old)
		_, err := pool.Exec(ctx, `UPDATE llm_evaluation_captures SET captured_at=clock_timestamp()-interval '2 minutes',expires_at=clock_timestamp()-interval '1 second';
UPDATE llm_evaluation_capture_admissions SET expires_at=clock_timestamp()-interval '1 second'`)
		require.NoError(t, err)
		_, err = store.DeleteExpired(ctx)
		require.NoError(t, err)
		_, err = recorder.Capture(ctx, req)
		require.NoError(t, err)
		_, current := bridgeCapture(t, ctx, recorder, req, "synthetic-build-new")
		current.Case = old.Case
		_, _, err = controller.Prepare(ctx, current)
		require.ErrorIs(t, err, ErrDispatchUnknown)
		replacement, err := recorder.RecordHTTPResult(ctx, old.CaptureKey, HTTPResult{Body: []byte(`{"first":true}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		require.Equal(t, original.ResponseSHA256, replacement.ResponseSHA256)
		_, _, err = controller.Prepare(ctx, current)
		require.ErrorIs(t, err, ErrDispatchUnknown)
		require.ErrorIs(t, controller.ObserveResult(ctx, DispatchLease{CaptureKey: old.CaptureKey, FenceKey: old.CaptureKey}, replacement), ErrCaptureUnavailable)
		recovery, err := controller.TaskRecovery(ctx, old.Case.TaskKey)
		require.NoError(t, err)
		require.Len(t, recovery, 1)
		require.False(t, recovery[0].Replayable)
		require.False(t, recovery[0].SafeToRetry)
		_, err = controller.Replay(ctx, current)
		require.ErrorIs(t, err, ErrDispatchUnknown)
		bridgeCounts(t, ctx, pool, 1, 1, 1)
	})

	t.Run("no dispatch follows current build and reuses original reservation", func(t *testing.T) {
		ctx, pool, _, recorder, controller, req, old := dispatchFixture(t)
		lease, _, err := controller.Prepare(ctx, old)
		require.NoError(t, err)
		allowed, err := controller.Reserve(ctx, lease, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		require.NoError(t, controller.DeferNoDispatch(ctx, lease, "canceled_before_intent", time.Now().Add(-time.Second)))
		_, current := bridgeCapture(t, ctx, recorder, req, "synthetic-build-new")
		fresh, outcome, err := controller.Prepare(ctx, current)
		require.NoError(t, err)
		require.Equal(t, DispatchPrepared, outcome)
		require.Equal(t, old.CaptureKey, fresh.FenceKey)
		require.Equal(t, current.CaptureKey, fresh.CaptureKey)
		require.Greater(t, fresh.Generation, lease.Generation)
		allowed, err = controller.Reserve(ctx, fresh, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		require.ErrorIs(t, controller.BeginDispatch(ctx, lease), ErrDispatchLease)
		require.NoError(t, controller.BeginDispatch(ctx, fresh))
		receipt, err := recorder.RecordHTTPResult(ctx, fresh.CaptureKey, HTTPResult{Body: []byte(`{"current":true}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		require.NoError(t, controller.ObserveResult(ctx, fresh, receipt))
		require.ErrorIs(t, controller.ObserveResult(ctx, lease, receipt), ErrCaptureUnavailable)
		_, outcome, err = controller.Prepare(ctx, old)
		require.NoError(t, err)
		require.Equal(t, DispatchReplay, outcome)
		replay, err := controller.Replay(ctx, old)
		require.NoError(t, err)
		require.Equal(t, current.CaptureKey, replay.Receipt.CaptureKey)
		var oldResult bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM llm_evaluation_capture_results WHERE capture_key=$1)`, old.CaptureKey).Scan(&oldResult))
		require.False(t, oldResult, "the old no-dispatch generation never becomes an HTTP observation")
		bridgeCounts(t, ctx, pool, 1, 1, 1)
	})

	t.Run("budget retry boundary remains binding after a build change", func(t *testing.T) {
		ctx, pool, _, recorder, controller, req, old := dispatchFixture(t)
		lease, _, err := controller.Prepare(ctx, old)
		require.NoError(t, err)
		until := time.Now().Add(time.Hour)
		require.NoError(t, controller.DeferNoDispatch(ctx, lease, "daily_budget", until))
		_, current := bridgeCapture(t, ctx, recorder, req, "synthetic-build-new")
		_, _, err = controller.Prepare(ctx, current)
		var deferred *DispatchDeferredError
		require.ErrorAs(t, err, &deferred)
		require.WithinDuration(t, until, deferred.RetryAfterUTC, time.Millisecond)
		bridgeCounts(t, ctx, pool, 1, 0, 0)
	})

	for _, body := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy without dispatch proof held with body=%t", body), func(t *testing.T) {
			ctx, pool, _, recorder, controller, req, old := dispatchFixture(t)
			results := 0
			if body {
				_, err := recorder.RecordHTTPResult(ctx, old.CaptureKey, HTTPResult{Body: []byte(`{"legacy":true}`), StatusCode: 200, ErrorClass: "none"})
				require.NoError(t, err)
				results = 1
			}
			_, current := bridgeCapture(t, ctx, recorder, req, "synthetic-build-new")
			current.Case = dispatchCaseFixture(t, ctx, pool, req)
			_, _, err := controller.Prepare(ctx, current)
			require.ErrorIs(t, err, ErrDispatchUnknown)
			_, err = controller.Replay(ctx, current)
			require.ErrorIs(t, err, ErrDispatchUnknown)
			// Repeated attempts and task recovery must not convert this durable
			// lack of dispatch proof into cross-build replay authority.
			_, _, err = controller.Prepare(ctx, current)
			require.ErrorIs(t, err, ErrDispatchUnknown)
			recovery, err := controller.TaskRecovery(ctx, current.Case.TaskKey)
			require.NoError(t, err)
			require.Len(t, recovery, 1)
			require.Equal(t, "unknown", recovery[0].State)
			require.False(t, recovery[0].Replayable)
			require.False(t, recovery[0].SafeToRetry)
			bridgeCounts(t, ctx, pool, 1, results, 0)
		})
	}

	t.Run("concurrent different build captures share one dispatch owner", func(t *testing.T) {
		ctx, pool, _, recorder, controller, req, old := dispatchFixture(t)
		lease, _, err := controller.Prepare(ctx, old)
		require.NoError(t, err)
		require.NoError(t, controller.DeferNoDispatch(ctx, lease, "not_dispatched", time.Now().Add(-time.Second)))
		bindings := make([]DispatchRequest, 12)
		for i := range bindings {
			_, bindings[i] = bridgeCapture(t, ctx, recorder, req, fmt.Sprintf("synthetic-build-%d", i))
		}
		var wg sync.WaitGroup
		leases := make(chan DispatchLease, len(bindings))
		errs := make(chan error, len(bindings))
		for _, binding := range bindings {
			wg.Add(1)
			go func() {
				defer wg.Done()
				lease, outcome, err := controller.Prepare(ctx, binding)
				if outcome == DispatchPrepared && err == nil {
					leases <- lease
				} else if err != ErrDispatchBusy {
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
		owner := <-leases
		require.Equal(t, old.CaptureKey, owner.FenceKey)
		allowed, err := controller.Reserve(ctx, owner, "matcher", 15, 450)
		require.NoError(t, err)
		require.True(t, allowed)
		require.NoError(t, controller.BeginDispatch(ctx, owner))
		receipt, err := recorder.RecordHTTPResult(ctx, owner.CaptureKey, HTTPResult{Body: []byte(`{"first":true}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		require.NoError(t, controller.ObserveResult(ctx, owner, receipt))
		for _, binding := range bindings {
			_, outcome, err := controller.Prepare(ctx, binding)
			require.NoError(t, err)
			require.Equal(t, DispatchReplay, outcome)
			replay, err := controller.Replay(ctx, binding)
			require.NoError(t, err)
			require.Equal(t, owner.CaptureKey, replay.Receipt.CaptureKey)
		}
		var aliases int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_capture_dispatch_aliases`).Scan(&aliases))
		require.Equal(t, 13, aliases)
		bridgeCounts(t, ctx, pool, 1, 1, 1)
	})

	dimensions := map[string]func(*Request){
		"model": func(r *Request) { r.Model += "-changed" }, "provider": func(r *Request) { r.Endpoint += "/changed" },
		"policy contract": func(r *Request) { r.ContractID += "-changed" }, "prompt version": func(r *Request) { r.PromptVersion += "-changed" },
		"system prompt": func(r *Request) { r.SystemPrompt += " changed" }, "model envelope": func(r *Request) { r.ModelInputJSON = []byte(`{"changed":true}`) },
		"task input": func(r *Request) { r.TaskInputJSON = []byte(`{"changed":true}`) }, "group": func(r *Request) { r.GroupKey = []byte("changed-group") },
		"candidate source": func(r *Request) { r.CandidateSource = CandidateSourceAPI },
		"task":             func(r *Request) { r.Task = TaskMatcherExtract; r.CandidateSource = CandidateSourceNone },
		"source hash":      func(r *Request) { r.InfoHash = bytes.Repeat([]byte{73}, 20) },
	}
	for name, mutate := range dimensions {
		t.Run(name+" is a distinct semantic request", func(t *testing.T) {
			ctx, pool, _, recorder, controller, req, old := dispatchFixture(t)
			bridgeComplete(t, ctx, recorder, controller, old)
			oldSemantic, err := SemanticKeyForRequest(req)
			require.NoError(t, err)
			mutate(&req)
			if name == "source hash" {
				_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,created_at,updated_at,files_status)
VALUES($1,'SyntheticDispatch',4096,false,now(),now(),'single')`, req.InfoHash)
				require.NoError(t, err)
			}
			_, current := bridgeCapture(t, ctx, recorder, req, "synthetic-build-new")
			require.NotEqual(t, oldSemantic, current.SemanticKey)
			lease, outcome, err := controller.Prepare(ctx, current)
			require.NoError(t, err)
			require.Equal(t, DispatchPrepared, outcome)
			require.Equal(t, current.CaptureKey, lease.FenceKey)
			require.Equal(t, current.CaptureKey, lease.CaptureKey)
			bridgeCounts(t, ctx, pool, 2, 1, 1)
		})
	}

	t.Run("caller digest cannot claim another capture's semantics", func(t *testing.T) {
		ctx, pool, _, _, controller, _, binding := dispatchFixture(t)
		binding.SemanticKey = bytes.Repeat([]byte{9}, sha256.Size)
		_, _, err := controller.Prepare(ctx, binding)
		require.ErrorIs(t, err, ErrCaptureUnavailable)
		bridgeCounts(t, ctx, pool, 0, 0, 0)
	})

	for _, change := range []string{"native privacy", "source evidence", "lease generation", "qB privacy"} {
		t.Run(change+" blocks historical replay after upgrade", func(t *testing.T) {
			ctx, pool, _, recorder, controller, req, old := dispatchFixture(t)
			old.Case = dispatchCaseFixture(t, ctx, pool, req)
			bridgeComplete(t, ctx, recorder, controller, old)
			_, current := bridgeCapture(t, ctx, recorder, req, "synthetic-build-new")
			current.Case = old.Case
			_, _, err := controller.Prepare(ctx, current)
			require.NoError(t, err)
			switch change {
			case "native privacy":
				_, err = pool.Exec(ctx, `UPDATE torrents SET private=true WHERE info_hash=$1`, req.InfoHash)
			case "source evidence":
				_, err = pool.Exec(ctx, `UPDATE torrents SET name='ChangedSource' WHERE info_hash=$1`, req.InfoHash)
			case "lease generation":
				_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET lease_generation=lease_generation+1`)
			case "qB privacy":
				_, err = pool.Exec(ctx, `INSERT INTO label_evidence(info_hash,source,source_kind,source_instance,source_object_id,category,observed_at,strength) VALUES($1,'qbittorrent','torrent','synthetic','synthetic','private',now(),100)`, req.InfoHash)
			}
			require.NoError(t, err)
			_, err = controller.Replay(ctx, current)
			require.Error(t, err)
			_, _, err = controller.Prepare(ctx, current)
			require.Error(t, err)
			bridgeCounts(t, ctx, pool, 1, 1, 1)
		})
	}
}
