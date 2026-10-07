package llmwork

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestPostgresNamePolicyDoesNotEnqueueDeniedSourceOrApplyRetainedResponse(t *testing.T) {
	s, pool := workFixture(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `ALTER TABLE torrents ADD COLUMN name text NOT NULL DEFAULT 'Allowed.Release.2024.mkv'; CREATE TABLE torrent_contents(info_hash bytea,content_type text);CREATE TABLE synthetic_outcome(id int primary key)`)
	require.NoError(t, err)
	p, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	d := draftFor(1)
	putPublic(t, pool, d)
	_, err = pool.Exec(ctx, `UPDATE torrents SET name='Synthetic.电影.ENG.mkv' WHERE info_hash=$1`, d.InfoHash)
	require.NoError(t, err)
	s.SetNamePolicy(p)
	_, err = s.Enqueue(ctx, d)
	require.ErrorIs(t, err, ErrHeld)
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM llm_work_tasks`).Scan(&n))
	require.Zero(t, n)
	// An allowed task may already have paid for and retained a response when
	// the policy becomes applicable. Its source/fence is never deleted or
	// refunded; the final application boundary must still refuse mutation.
	s.SetNamePolicy(nil)
	_, err = pool.Exec(ctx, `UPDATE torrents SET name='Allowed.Release.2024.mkv' WHERE info_hash=$1`, d.InfoHash)
	require.NoError(t, err)
	_, err = s.Enqueue(ctx, d)
	require.NoError(t, err)
	l, err := s.Claim(ctx, "owned-name-policy")
	require.NoError(t, err)
	require.NotNil(t, l)
	_, err = pool.Exec(ctx, `UPDATE torrents SET name='Synthetic.Фильм.English.mkv' WHERE info_hash=$1`, d.InfoHash)
	require.NoError(t, err)
	s.SetNamePolicy(p)
	called := false
	err = s.Apply(ctx, *l, "retained-result", func(tx pgx.Tx) error {
		called = true
		_, x := tx.Exec(ctx, `INSERT INTO synthetic_outcome VALUES(1)`)
		return x
	})
	require.ErrorIs(t, err, ErrHeld)
	require.False(t, called)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM synthetic_outcome`).Scan(&n))
	require.Zero(t, n)
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_work_tasks WHERE task_key=$1`, l.Task.Key).Scan(&state))
	require.Equal(t, "leased", state)
	require.NoError(t, s.Finish(ctx, *l, "held", "name_policy", time.Now()))
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_work_tasks WHERE task_key=$1`, l.Task.Key).Scan(&state))
	require.Equal(t, "held", state)
}
