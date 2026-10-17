package processors

import (
	"context"
	"testing"
	"time"

	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func TestPostgresPreparationTimeoutRetainsInputAndRecoversAfterRestart(t *testing.T) {
	s, p, _ := fixture(t)
	service := newService(t, "prepare", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	register(t, s, service)
	h := seedTorrent(t, p, 94, nil)
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	var id, revision, source string
	require.NoError(t, p.QueryRow(testContext, `SELECT job_id,input_revision,source::text FROM processor_jobs`).Scan(&id, &revision, &source))

	// Claim only touches queue tables. Block the subsequent catalogue read so
	// this is a real PostgreSQL preparation timeout before any HTTP disclosure.
	lock, err := p.Begin(testContext)
	require.NoError(t, err)
	defer rollback(lock)
	_, err = lock.Exec(testContext, `LOCK TABLE torrents IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	s.cfg.RequestTimeout = 100 * time.Millisecond
	worked, err := s.RunOne(testContext)
	require.True(t, worked)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, service.calls.Load())
	exported, err := s.ExportJob(testContext, id)
	require.NoError(t, err)
	require.Equal(t, "deferred", exported.Job.State)
	require.Equal(t, "failed", exported.Job.Outcome)
	require.Equal(t, "preparation_failed", exported.Job.Reason)
	require.Equal(t, 1, exported.Job.PreparationFailures)
	require.Zero(t, exported.Job.Attempts)
	require.Empty(t, exported.Attempts)
	require.Nil(t, exported.LatestApplication)
	require.Nil(t, exported.LatestRestriction)
	require.Equal(t, revision, exported.Job.InputRevision)
	require.JSONEq(t, source, string(exported.SourceSnapshot))
	require.NoError(t, lock.Rollback(testContext))

	s.cfg.RequestTimeout = NewDefaultConfig().RequestTimeout
	restarted, err := NewStore(s.cfg, s.pool, s.policy)
	require.NoError(t, err)
	_, err = p.Exec(testContext, `UPDATE processor_jobs SET available_at=clock_timestamp() WHERE job_id=$1`, id)
	require.NoError(t, err)
	run(t, restarted)
	exported, err = restarted.ExportJob(testContext, id)
	require.NoError(t, err)
	require.Equal(t, "completed", exported.Job.State)
	require.Equal(t, 1, exported.Job.PreparationFailures)
	require.Equal(t, 1, exported.Job.Attempts)
	require.Len(t, exported.Attempts, 1)
	require.EqualValues(t, 1, service.calls.Load())
	var kind string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind))
	require.Equal(t, "movie", kind)
	status, err := restarted.Status(testContext)
	require.NoError(t, err)
	require.Equal(t, 1.0, *status.Queue["preparation_failures"].Value)
	require.Equal(t, 1.0, *status.Processors[0].Metrics["technical_failure_prefix_attempts"].Value)
}

func TestPostgresPreparationFailureBudgetPreservesCatalogueAndEvidence(t *testing.T) {
	s, p, migrations := fixture(t)
	service := newService(t, "prepare-budget", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "tv_show") })
	register(t, s, service)
	h := seedTorrent(t, p, 95, "movie")
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	lock, err := p.Begin(testContext)
	require.NoError(t, err)
	defer rollback(lock)
	_, err = lock.Exec(testContext, `LOCK TABLE torrents IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	s.cfg.RequestTimeout = 100 * time.Millisecond
	for range s.cfg.MaxAttempts {
		worked, failure := s.RunOne(testContext)
		require.True(t, worked)
		require.ErrorIs(t, failure, context.DeadlineExceeded)
		_, err = p.Exec(testContext, `UPDATE processor_jobs SET available_at=clock_timestamp() WHERE state='deferred'`)
		require.NoError(t, err)
	}
	require.NoError(t, lock.Rollback(testContext))
	worked, err := s.RunOne(testContext)
	require.NoError(t, err)
	require.False(t, worked)
	require.Zero(t, service.calls.Load())
	var state, outcome, reason, kind string
	var failures, attempts int
	require.NoError(t, p.QueryRow(testContext, `SELECT state,outcome,reason,preparation_failures,attempts FROM processor_jobs`).Scan(&state, &outcome, &reason, &failures, &attempts))
	require.Equal(t, "failed", state)
	require.Equal(t, "failed", outcome)
	require.Equal(t, "preparation_failed", reason)
	require.Equal(t, s.cfg.MaxAttempts, failures)
	require.Zero(t, attempts)
	for _, table := range []string{"processor_attempts", "processor_applications", "processor_exclusions"} {
		var count int
		require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM `+table).Scan(&count))
		require.Zero(t, count)
	}
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind))
	require.Equal(t, "movie", kind)
	_, err = migrations.DownTo(testContext, 65)
	require.ErrorContains(t, err, "preparation failure evidence prevents destructive downgrade")
}

func TestPostgresPreparationRetryCannotReviveCanceledLease(t *testing.T) {
	s, p, _ := fixture(t)
	register(t, s, newService(t, "prepare-cancel", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") }))
	seedTorrent(t, p, 96, nil)
	_, err := s.Scan(testContext)
	require.NoError(t, err)
	lease, err := s.Claim(testContext)
	require.NoError(t, err)
	require.NotNil(t, lease)
	_, err = s.Cancel(testContext, lease.ID)
	require.NoError(t, err)
	require.ErrorIs(t, s.failPreparation(testContext, *lease), ErrStale)
	var state string
	var failures int
	require.NoError(t, p.QueryRow(testContext, `SELECT state,preparation_failures FROM processor_jobs WHERE job_id=$1`, lease.ID).Scan(&state, &failures))
	require.Equal(t, "canceled", state)
	require.Zero(t, failures)
}
