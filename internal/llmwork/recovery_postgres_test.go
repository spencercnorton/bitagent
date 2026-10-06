package llmwork

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
	"github.com/stretchr/testify/require"
)

type recoveryControl struct {
	llmcapture.DispatchControl
	attempts []llmcapture.DispatchRecovery
}

func (recoveryControl) Enabled() bool { return true }
func (r recoveryControl) TaskRecovery(context.Context, []byte) ([]llmcapture.DispatchRecovery, error) {
	return r.attempts, nil
}

type queryingRecovery struct {
	recoveryControl
	pool *pgxpool.Pool
}

func (r queryingRecovery) TaskRecovery(ctx context.Context, _ []byte) ([]llmcapture.DispatchRecovery, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM llm_capture_dispatch_attempts`).Scan(&n)
	return nil, err
}

func TestLeaseRecoveryUsesAOneConnectionPool(t *testing.T) {
	s, pool := workFixture(t)
	ctx := context.Background()
	d := draftFor(1)
	putPublic(t, pool, d)
	_, err := s.Enqueue(ctx, d)
	require.NoError(t, err)
	old, err := s.Claim(ctx, "crash")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET lease_until=now()-interval '1 second' WHERE task_key=$1`, old.Task.Key)
	require.NoError(t, err)
	cfg := pool.Config().Copy()
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer single.Close()
	s.pool = lazy.New(func() (*pgxpool.Pool, error) { return single, nil })
	s.SetDispatch(queryingRecovery{pool: single})
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	recovered, err := s.Claim(deadline, "resume")
	require.NoError(t, err)
	require.NotNil(t, recovered)
}

func TestExpiredTaskRecoveryAllowsOnlyProvenDispatchStates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts []llmcapture.DispatchRecovery
		safe     bool
	}{
		{"no_attempt", nil, true},
		{"not_dispatched", []llmcapture.DispatchRecovery{{State: "no_dispatch", SafeToRetry: true}}, true},
		{"retained_first_response", []llmcapture.DispatchRecovery{{State: "result", Replayable: true}}, true},
		{"transport_unknown", []llmcapture.DispatchRecovery{{State: "unknown"}}, false},
		{"intent", []llmcapture.DispatchRecovery{{State: "intent"}}, false},
		{"expired_response", []llmcapture.DispatchRecovery{{State: "result"}}, false},
		{"mixed_stage_hold", []llmcapture.DispatchRecovery{{State: "result", Replayable: true}, {State: "unknown"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pool := workFixture(t)
			ctx := context.Background()
			d := draftFor(1)
			d.Kind = Matcher
			putPublic(t, pool, d)
			_, err := s.Enqueue(ctx, d)
			require.NoError(t, err)
			old, err := s.Claim(ctx, "crashed")
			require.NoError(t, err)
			require.NotNil(t, old)
			_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET lease_until=now()-interval '1 second' WHERE task_key=$1`, old.Task.Key)
			require.NoError(t, err)
			s.SetDispatch(recoveryControl{attempts: tc.attempts})
			next, err := s.Claim(ctx, "restarted")
			require.NoError(t, err)
			if tc.safe {
				require.NotNil(t, next)
				require.Greater(t, next.Generation, old.Generation)
				if tc.name == "retained_first_response" {
					require.Equal(t, 100, next.Task.Priority, "started matcher completion retains priority across restart")
				}
				require.ErrorIs(t, s.Heartbeat(ctx, *old), ErrLease)
			} else {
				require.Nil(t, next)
				var state string
				require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_work_tasks WHERE task_key=$1`, old.Task.Key).Scan(&state))
				require.Equal(t, "held", state)
			}
		})
	}
}

func TestTaskCleanupPreservesPermanentFenceAndCommittedApplication(t *testing.T) {
	s, pool := workFixture(t)
	ctx := context.Background()
	d := draftFor(1)
	putPublic(t, pool, d)
	_, err := s.Enqueue(ctx, d)
	require.NoError(t, err)
	key, err := d.Key()
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO llm_capture_dispatch_attempts(capture_key,task,task_key,state,reason)VALUES($1,'classifier_type',$2,'unknown','transport')`, Digest("unknown_capture"), key)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET state='held',created_at=now()-interval '8 days',expires_at=now()-interval '1 day' WHERE task_key=$1`, key)
	require.NoError(t, err)
	n, err := s.Cleanup(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_work_tasks`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_capture_dispatch_attempts WHERE state='unknown'`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = s.Enqueue(ctx, d)
	require.NoError(t, err)
	lease, err := s.Claim(ctx, "apply")
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.NoError(t, s.Apply(ctx, *lease, "applied", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO llm_work_applications(task_key,info_hash,source_digest,policy_digest,applied_snapshot) VALUES($1,$2,$3,$4,'{"schema":"synthetic"}'::jsonb)`, key, d.InfoHash, d.SourceDigest, d.PolicyDigest)
		return err
	}))
	_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET created_at=now()-interval '8 days',expires_at=now()-interval '1 day' WHERE task_key=$1`, key)
	require.NoError(t, err)
	n, err = s.Cleanup(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	var payload string
	require.NoError(t, pool.QueryRow(ctx, `SELECT payload::text FROM llm_work_tasks WHERE task_key=$1`, key).Scan(&payload))
	require.Equal(t, "{}", payload)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_work_applications`).Scan(&count))
	require.Equal(t, 1, count)
	n, err = s.Cleanup(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "scrubbed applications must not starve later cleanup batches")
}

func TestApplyRollsBackWhenLeaseExpiresDuringCallback(t *testing.T) {
	s, pool := workFixture(t)
	ctx := context.Background()
	d := draftFor(1)
	putPublic(t, pool, d)
	_, err := s.Enqueue(ctx, d)
	require.NoError(t, err)
	l, err := s.Claim(ctx, "apply")
	require.NoError(t, err)
	require.NotNil(t, l)
	_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET lease_until=clock_timestamp()+interval '100 milliseconds' WHERE task_key=$1`, l.Task.Key)
	require.NoError(t, err)
	err = s.Apply(ctx, *l, "applied", func(tx pgx.Tx) error {
		_, x := tx.Exec(ctx, `INSERT INTO llm_work_applications(task_key,info_hash,source_digest,policy_digest,applied_snapshot)VALUES($1,$2,$3,$4,'{}'::jsonb)`, l.Task.Key, d.InfoHash, d.SourceDigest, d.PolicyDigest)
		if x != nil {
			return x
		}
		_, x = tx.Exec(ctx, `SELECT pg_sleep(0.2)`)
		return x
	})
	require.ErrorIs(t, err, ErrLease)
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_work_applications`).Scan(&n))
	require.Zero(t, n)
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_work_tasks`).Scan(&state))
	require.Equal(t, "leased", state)
}

func TestPausedQueueStillExpiresRawProposals(t *testing.T) {
	s, pool := workFixture(t)
	ctx := context.Background()
	d := draftFor(1)
	putPublic(t, pool, d)
	_, err := s.Enqueue(ctx, d)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET created_at=now()-interval '8 days',expires_at=now()-interval '1 day'`)
	require.NoError(t, err)
	s.cfg.Enabled = false
	n, err := s.Cleanup(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_work_tasks`).Scan(&count))
	require.Zero(t, count)
}
