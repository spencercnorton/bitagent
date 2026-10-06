package llmwork

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// recoverExpired reconciles expired generations without holding a connection
// during the controller's reads. A conditional update protects renewed or
// concurrently recovered leases. A new generation can resume only when all
// attempted requests are positively undispatched or have retained responses.
func (s *Store) recoverExpired(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `SELECT task_key,lease_generation FROM llm_work_tasks
 WHERE state='leased' AND lease_until<=clock_timestamp()
 ORDER BY lease_until,task_key LIMIT 64`)
	if err != nil {
		return err
	}
	type expired struct {
		key        []byte
		generation int64
	}
	var keys []expired
	for rows.Next() {
		var item expired
		if err = rows.Scan(&item.key, &item.generation); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range keys {
		state, reason := "held", "expired_lease_needs_reconcile"
		if s.dispatch != nil && s.dispatch.Enabled() {
			attempts, err := s.dispatch.TaskRecovery(ctx, item.key)
			if err != nil {
				return fmt.Errorf("recover optional model dispatch: %w", err)
			}
			safe := true
			for _, a := range attempts {
				if !a.SafeToRetry && !a.Replayable {
					safe = false
					break
				}
			}
			if safe {
				state, reason = "deferred", "recovered_before_dispatch_or_replay"
			} else {
				reason = "unknown_dispatch_or_expired_response"
			}
		}
		if _, err = pool.Exec(ctx, `WITH changed AS(UPDATE llm_work_tasks SET state=$2,reason=$3,lease_owner=NULL,lease_until=NULL,retry_after=transaction_timestamp()
 WHERE task_key=$1 AND state='leased' AND lease_generation=$4 AND lease_until<=clock_timestamp() RETURNING task_key)
 INSERT INTO llm_work_events(task_key,state,reason) SELECT task_key,$2,$3 FROM changed`, item.key, state, reason, item.generation); err != nil {
			return err
		}
	}
	return nil
}

// Completed proves that a successful handler committed task application under
// this exact generation. Receiving a model response alone is insufficient.
func (s *Store) Completed(ctx context.Context, l Lease) (bool, error) {
	pool, err := s.pool.Get()
	if err != nil {
		return false, err
	}
	var done bool
	err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM llm_work_tasks WHERE task_key=$1 AND state='completed' AND lease_generation=$2)`, l.Task.Key, l.Generation).Scan(&done)
	return done, err
}

// Cleanup bounds expired proposal data and event history. Application digests
// and snapshots are retained for source-bound preservation; request payloads
// are scrubbed. The permanent dispatch table is never deleted or refunded.
func (s *Store) Cleanup(ctx context.Context) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	var pool *pgxpool.Pool
	err := s.pool.IfInitialized(func(p *pgxpool.Pool) error { pool = p; return nil })
	if err != nil || pool == nil {
		return 0, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// Turning off inference does not suspend raw proposal retention. These
	// bounded age transitions cannot grant dispatch or retry authority.
	if _, err = tx.Exec(ctx, `WITH aged AS(SELECT task_key FROM llm_work_tasks
 WHERE expires_at<=clock_timestamp() AND (state IN ('queued','deferred') OR state='leased' AND lease_until<=clock_timestamp())
 ORDER BY expires_at,task_key FOR UPDATE SKIP LOCKED LIMIT 128), changed AS(
 UPDATE llm_work_tasks SET state=CASE WHEN state='leased' THEN 'held' ELSE 'expired' END,
 reason='task_age',lease_owner=NULL,lease_until=NULL WHERE task_key IN(SELECT task_key FROM aged) RETURNING task_key,state)
 INSERT INTO llm_work_events(task_key,state,reason) SELECT task_key,state,'task_age' FROM changed`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT task_key FROM llm_work_tasks w WHERE state IN ('held','obsolete','expired','completed') AND expires_at<=clock_timestamp()
 AND (payload<>'{}'::jsonb OR EXISTS(SELECT 1 FROM llm_work_events e WHERE e.task_key=w.task_key)
      OR NOT EXISTS(SELECT 1 FROM llm_work_applications a WHERE a.task_key=w.task_key))
 ORDER BY expires_at,task_key FOR UPDATE SKIP LOCKED LIMIT 128`)
	if err != nil {
		return 0, err
	}
	var keys [][]byte
	for rows.Next() {
		var key []byte
		if err = rows.Scan(&key); err != nil {
			rows.Close()
			return 0, err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, key := range keys {
		if _, err = tx.Exec(ctx, `DELETE FROM llm_work_events WHERE task_key=$1`, key); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `UPDATE llm_work_tasks SET payload='{}'::jsonb WHERE task_key=$1 AND EXISTS(SELECT 1 FROM llm_work_applications a WHERE a.task_key=$1)`, key); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM llm_work_tasks WHERE task_key=$1 AND NOT EXISTS(SELECT 1 FROM llm_work_applications a WHERE a.task_key=$1)`, key); err != nil {
			return 0, err
		}
	}
	return int64(len(keys)), tx.Commit(ctx)
}

func (s *Store) RefreshMetrics(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	rows, err := pool.Query(ctx, `SELECT kind,state,count(*) FROM llm_work_tasks GROUP BY kind,state`)
	if err != nil {
		return err
	}
	defer rows.Close()
	s.metrics.depth.Reset()
	for rows.Next() {
		var kind, state string
		var n int
		if err = rows.Scan(&kind, &state, &n); err != nil {
			return err
		}
		s.metrics.depth.WithLabelValues(kind, state).Set(float64(n))
	}
	return rows.Err()
}
