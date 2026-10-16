package processors

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func windowJob(t *testing.T, pool *pgxpool.Pool, digit string) string {
	t.Helper()
	id := strings.Repeat(digit, 64)
	_, err := pool.Exec(testContext, `INSERT INTO processor_jobs
 (job_id,info_hash,input_revision,pipeline_revision,epoch,input,source,plan,state,finished_at)
 VALUES($1,decode(repeat('11',20),'hex'),repeat('a',64),repeat('b',64),1,'{}','{}','{}','completed',clock_timestamp())`, id)
	require.NoError(t, err)
	return id
}

func TestPostgresExactTechnicalFailureWindow(t *testing.T) {
	s, pool, _ := fixture(t)
	service := newService(t, "window", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	entry := register(t, s, service)
	status, err := s.Status(testContext)
	require.NoError(t, err)
	require.Nil(t, status.Processors[0].Metrics["technical_failure_window_failures"].Value)
	require.Equal(t, "insufficient_attempt_history", status.Processors[0].Metrics["technical_failure_window_attempts"].UnavailableReason)
	id := windowJob(t, pool, "a")
	_, err = pool.Exec(testContext, `INSERT INTO processor_attempts
 (job_id,lease_generation,processor_id,generation,stage,queue_wait_ms,finished_at,outcome)
 SELECT $1,i,$2,$3,'classify',0,clock_timestamp()+i*interval '1 microsecond',
 CASE WHEN i<=7 THEN 'failed' WHEN i%3=0 THEN 'abstained' WHEN i%3=1 THEN 'unmatched' ELSE 'deferred' END
 FROM generate_series(1,501) i`, id, entry.ID, entry.ExecutionGeneration)
	require.NoError(t, err)
	// Newer rows from another execution must not alter this sample.
	_, err = pool.Exec(testContext, `INSERT INTO processor_attempts
 (job_id,lease_generation,processor_id,generation,stage,queue_wait_ms,finished_at,outcome)
 SELECT $1,i,$2,$3,'classify',0,clock_timestamp(),'failed' FROM generate_series(502,511) i`, id, entry.ID, entry.ExecutionGeneration+999)
	require.NoError(t, err)
	status, err = s.Status(testContext)
	require.NoError(t, err)
	m := status.Processors[0].Metrics
	require.Equal(t, 500.0, *m["technical_failure_window_attempts"].Value)
	require.Equal(t, 6.0, *m["technical_failure_window_failures"].Value)
	require.Zero(t, *m["technical_failure_window_unknown"].Value)
	require.Empty(t, m["technical_failure_window_failures"].UnavailableReason)

	// A retained row count alone is not completeness after retention or upgrade.
	_, err = pool.Exec(testContext, `UPDATE processor_control SET attempt_history_floor=clock_timestamp()`)
	require.NoError(t, err)
	status, err = s.Status(testContext)
	require.NoError(t, err)
	require.Nil(t, status.Processors[0].Metrics["technical_failure_window_failures"].Value)
	require.Equal(t, "retention_history_incomplete", status.Processors[0].Metrics["technical_failure_window_attempts"].UnavailableReason)

	id2 := windowJob(t, pool, "b")
	_, err = pool.Exec(testContext, `INSERT INTO processor_attempts
 (job_id,lease_generation,processor_id,generation,stage,queue_wait_ms,finished_at,outcome)
 SELECT $1,i,$2,$3,'classify',0,clock_timestamp()+i*interval '1 microsecond','completed'
 FROM generate_series(1,500) i`, id2, entry.ID, entry.ExecutionGeneration)
	require.NoError(t, err)
	status, err = s.Status(testContext)
	require.NoError(t, err)
	require.Equal(t, 0.0, *status.Processors[0].Metrics["technical_failure_window_failures"].Value)

	_, err = pool.Exec(testContext, `INSERT INTO processor_attempts
 (job_id,lease_generation,processor_id,generation,stage,queue_wait_ms,finished_at,outcome)
 VALUES($1,501,$2,$3,'classify',0,clock_timestamp(),'unknown'),
 ($1,502,$2,$3,'classify',0,NULL,'')`, id2, entry.ID, entry.ExecutionGeneration)
	require.NoError(t, err)
	status, err = s.Status(testContext)
	require.NoError(t, err)
	m = status.Processors[0].Metrics
	require.Equal(t, 500.0, *m["technical_failure_window_attempts"].Value)
	require.Nil(t, m["technical_failure_window_failures"].Value)
	require.Equal(t, "unresolved_attempt_outcomes", m["technical_failure_window_failures"].UnavailableReason)
	require.Equal(t, 1.0, *m["technical_failure_window_unknown"].Value)
	require.Equal(t, 1.0, *m["technical_failure_window_unfinished"].Value)
}

func TestPostgresRetentionAdvancesAttemptCompletenessFloor(t *testing.T) {
	s, pool, _ := fixture(t)
	service := newService(t, "retention", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	entry := register(t, s, service)
	id := windowJob(t, pool, "c")
	_, err := pool.Exec(testContext, `UPDATE processor_control SET attempt_history_floor=clock_timestamp()-interval '3 hours';
 UPDATE processor_jobs SET finished_at=clock_timestamp()-interval '2 hours' WHERE job_id=$1;
 INSERT INTO processor_attempts(job_id,lease_generation,processor_id,generation,stage,queue_wait_ms,finished_at,outcome)
 VALUES($1,1,$2,$3,'classify',0,clock_timestamp()-interval '2 hours','failed')`, id, entry.ID, entry.ExecutionGeneration)
	require.NoError(t, err)
	var removedAt time.Time
	require.NoError(t, pool.QueryRow(testContext, `SELECT finished_at FROM processor_attempts WHERE job_id=$1`, id).Scan(&removedAt))
	s.cfg.Retention = time.Hour
	_, err = s.Scan(testContext)
	require.NoError(t, err)
	var floor time.Time
	var count int
	require.NoError(t, pool.QueryRow(testContext, `SELECT attempt_history_floor FROM processor_control`).Scan(&floor))
	require.False(t, floor.Before(removedAt))
	require.NoError(t, pool.QueryRow(testContext, `SELECT count(*) FROM processor_attempts WHERE job_id=$1`, id).Scan(&count))
	require.Zero(t, count)
}

func TestPostgresInterruptedAttemptsAreNotSuccessful(t *testing.T) {
	s, pool, _ := fixture(t)
	service := newService(t, "interruptions", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	register(t, s, service)
	seedTorrent(t, pool, 91, nil)
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	lease, err := s.Claim(testContext)
	require.NoError(t, err)
	require.NotNil(t, lease)
	_, err = s.Prepare(testContext, *lease)
	require.NoError(t, err)
	require.NoError(t, s.startAttempt(testContext, *lease))
	_, err = pool.Exec(testContext, `UPDATE processor_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE job_id=$1`, lease.ID)
	require.NoError(t, err)
	retry, err := s.Claim(testContext)
	require.NoError(t, err)
	require.NotNil(t, retry)
	var outcome, reason string
	require.NoError(t, pool.QueryRow(testContext, `SELECT outcome,reason FROM processor_attempts WHERE job_id=$1 AND lease_generation=$2`, lease.ID, lease.Generation).Scan(&outcome, &reason))
	require.Equal(t, "failed", outcome)
	require.Equal(t, "lease_expired", reason)
	_, err = s.Prepare(testContext, *retry)
	require.NoError(t, err)
	require.NoError(t, s.startAttempt(testContext, *retry))
	_, err = s.Cancel(testContext, retry.ID)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(testContext, `SELECT outcome,reason FROM processor_attempts WHERE job_id=$1 AND lease_generation=$2`, retry.ID, retry.Generation).Scan(&outcome, &reason))
	require.Equal(t, "unknown", outcome)
	require.Equal(t, "operator_canceled", reason)
	status, err := s.Status(testContext)
	require.NoError(t, err)
	require.Equal(t, 0.0, *status.Processors[0].Metrics["technical_failure_window_unfinished"].Value)
	require.Equal(t, 2.0, *status.Processors[0].Metrics["technical_failure_window_progress"].Value)
}
