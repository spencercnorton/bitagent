package llmwork

import (
	"context"
	"github.com/jackc/pgx/v5"
	"testing"

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
