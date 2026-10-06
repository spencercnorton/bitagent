package llmwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmcapture"
)

type Store struct {
	cfg      Config
	pool     lazy.Lazy[*pgxpool.Pool]
	dispatch llmcapture.DispatchControl
	metrics  *Metrics
}

func NewStore(cfg Config, pool lazy.Lazy[*pgxpool.Pool]) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Store{cfg: cfg, pool: pool, metrics: NewMetrics()}, nil
}
func (s *Store) Enabled() bool  { return s != nil && s.cfg.Enabled }
func (s *Store) Config() Config { return s.cfg }

// SetDispatch is startup-only wiring. Automatic lease recovery requires the
// same controller that fences every provider boundary for these tasks.
func (s *Store) SetDispatch(d llmcapture.DispatchControl) { s.dispatch = d }

const publicSourceSQL = `EXISTS (
 SELECT 1 FROM torrents t WHERE t.info_hash=$1 AND t.private=false
 AND NOT EXISTS (SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash
   AND e.source='qbittorrent' AND lower(e.category) IN ('private','bitgrab'))
)`

// Enqueue bounds both storage and database waiting. A full/unavailable queue
// never asks ingestion to wait for model capacity or to repeat classification.
// Time-bucket caps reserve space for traffic after the first hour of a day.
func (s *Store) Enqueue(ctx context.Context, d Draft) (outcome string, retErr error) {
	if !s.Enabled() {
		return "disabled", nil
	}
	defer func() {
		kind := d.Kind
		if kind != Type && kind != Language && kind != Matcher {
			kind = "unknown"
		}
		s.metrics.outcomes.WithLabelValues(string(kind), outcome).Inc()
	}()
	key, err := d.Key()
	if err != nil {
		return "invalid", err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.EnqueueTimeout)
	defer cancel()
	var pool *pgxpool.Pool
	if s.pool == nil {
		return "unavailable", fmt.Errorf("queue pool is unavailable")
	}
	err = s.pool.IfInitialized(func(p *pgxpool.Pool) error { pool = p; return nil })
	if err != nil || pool == nil {
		return "unavailable", fmt.Errorf("queue pool is not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "unavailable", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('llm_work.'||$1))`, string(d.Kind)); err != nil {
		return "unavailable", err
	}
	var public bool
	if err = tx.QueryRow(ctx, `SELECT `+publicSourceSQL, d.InfoHash).Scan(&public); err != nil {
		return "unavailable", err
	}
	if !public {
		return "privacy", ErrObsolete
	}
	var existing string
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT state FROM llm_work_tasks WHERE task_key=$1),'')`, key).Scan(&existing); err != nil {
		return "unavailable", err
	}
	if existing != "" {
		if existing == "completed" || existing == "held" || existing == "obsolete" || existing == "expired" {
			return existing, tx.Commit(ctx)
		}
		return "duplicate", tx.Commit(ctx)
	}
	var pending, bucketPending int
	var sameFamily bool
	if err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE time_bucket=floor(extract(hour FROM now() AT TIME ZONE 'UTC')*$2/24)::int),
 COALESCE(bool_or(family_digest=$3),false) FROM llm_work_tasks
 WHERE kind=$1 AND state IN ('queued','deferred','leased') AND expires_at>now()`, string(d.Kind), s.cfg.TimeBuckets, d.FamilyDigest).
		Scan(&pending, &bucketPending, &sameFamily); err != nil {
		return "unavailable", err
	}
	if sameFamily {
		return "family_sampled", tx.Commit(ctx)
	}
	if pending >= s.cfg.MaxPending || bucketPending >= (s.cfg.MaxPending+s.cfg.TimeBuckets-1)/s.cfg.TimeBuckets {
		return "full", tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO llm_work_tasks(task_key,kind,info_hash,source_digest,policy_digest,input_digest,family_digest,payload,
 priority,time_bucket,daily_limit,monthly_limit,state,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,floor(extract(hour FROM now() AT TIME ZONE 'UTC')*$10/24)::int,$11,$12,'queued',now()+make_interval(secs=>$13))`,
		key, string(d.Kind), d.InfoHash, d.SourceDigest, d.PolicyDigest, d.InputDigest, d.FamilyDigest, string(d.Payload), d.Priority, s.cfg.TimeBuckets, d.DailyLimit, d.MonthlyLimit, s.cfg.MaxTaskAge.Seconds()); err != nil {
		return "unavailable", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO llm_work_events(task_key,state,reason) VALUES($1,'queued','admitted_to_queue')`, key); err != nil {
		return "unavailable", err
	}
	return "queued", tx.Commit(ctx)
}

const taskColumns = `task_key,kind,info_hash,source_digest,policy_digest,input_digest,family_digest,payload::text,priority,daily_limit,monthly_limit,state,reason,created_at,retry_after,expires_at`

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	var kind, payload string
	err := row.Scan(&t.Key, &kind, &t.InfoHash, &t.SourceDigest, &t.PolicyDigest, &t.InputDigest, &t.FamilyDigest, &payload, &t.Priority, &t.DailyLimit, &t.MonthlyLimit, &t.State, &t.Reason, &t.CreatedAt, &t.RetryAfter, &t.ExpiresAt)
	t.Kind = Kind(kind)
	t.Payload = json.RawMessage(payload)
	return t, err
}

// Claim uses database UTC pacing under the unchanged scope allowance. Already
// partially completed cases take precedence; new cases are spread over the day.
func (s *Store) Claim(ctx context.Context, owner string) (*Lease, error) {
	if !s.Enabled() || !s.cfg.WorkerEnabled {
		return nil, nil
	}
	if owner == "" || len(owner) > 128 {
		return nil, ErrLease
	}
	pool, err := s.pool.Get()
	if err != nil {
		return nil, err
	}
	// Reconciliation may use the same bounded pool. Do it before acquiring
	// the claim transaction so a one-connection pool cannot deadlock.
	if err = s.recoverExpired(ctx, pool); err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `WITH aged AS(SELECT task_key FROM llm_work_tasks WHERE state IN ('queued','deferred') AND expires_at<=now()
 ORDER BY expires_at,task_key FOR UPDATE SKIP LOCKED LIMIT 128),
 changed AS(UPDATE llm_work_tasks SET state='expired',reason='task_age',lease_owner=NULL,lease_until=NULL
 WHERE task_key IN(SELECT task_key FROM aged) RETURNING task_key)
 INSERT INTO llm_work_events(task_key,state,reason) SELECT task_key,'expired','task_age' FROM changed`); err != nil {
		return nil, err
	}
	task, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM llm_work_tasks w
 WHERE state IN ('queued','deferred') AND retry_after<=now() AND expires_at>now()
 AND EXISTS(SELECT 1 FROM torrents t WHERE t.info_hash=w.info_hash AND t.private=false
   AND NOT EXISTS(SELECT 1 FROM label_evidence e WHERE e.info_hash=t.info_hash AND e.source='qbittorrent' AND lower(e.category) IN ('private','bitgrab')))
 AND (NOT $1 OR priority>0 OR daily_limit>0 AND now()>=
   ((now() AT TIME ZONE 'UTC')::date AT TIME ZONE 'UTC') + interval '1 day' * COALESCE((SELECT CASE WHEN b.day_start=(now() AT TIME ZONE 'UTC')::date THEN b.daily_calls ELSE 0 END::float8
      FROM llm_request_budgets b WHERE b.scope=w.kind AND b.month_start=date_trunc('month',now() AT TIME ZONE 'UTC')::date),0)/daily_limit)
 ORDER BY priority DESC,
   (SELECT max(done.completed_at) FROM llm_work_tasks done WHERE done.kind=w.kind AND done.time_bucket=w.time_bucket AND done.completed_at>now()-interval '1 day') ASC NULLS FIRST,
   created_at,task_key FOR UPDATE OF w SKIP LOCKED LIMIT 1`, s.cfg.SpreadAdmission))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	lease := &Lease{Task: task, Owner: owner}
	if err = tx.QueryRow(ctx, `UPDATE llm_work_tasks SET state='leased',reason='',lease_owner=$2,lease_generation=lease_generation+1,
 lease_until=now()+make_interval(secs=>$3) WHERE task_key=$1 RETURNING lease_generation,lease_until`, task.Key, owner, s.cfg.LeaseDuration.Seconds()).Scan(&lease.Generation, &lease.Until); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO llm_work_events(task_key,state,reason) VALUES($1,'leased','owned_worker')`, task.Key); err != nil {
		return nil, err
	}
	return lease, tx.Commit(ctx)
}

func (s *Store) Finish(ctx context.Context, l Lease, state, reason string, retryAfter time.Time) error {
	if state != "deferred" && state != "completed" && state != "held" && state != "obsolete" {
		return fmt.Errorf("invalid terminal lifecycle state")
	}
	if len(reason) > 64 {
		return fmt.Errorf("invalid lifecycle reason")
	}
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	var changed bool
	err = pool.QueryRow(ctx, `WITH changed AS(UPDATE llm_work_tasks SET state=$4,reason=$5,retry_after=$6,
		priority=priority,
 completed_at=CASE WHEN $4='completed' THEN now() ELSE completed_at END,lease_owner=NULL,lease_until=NULL
 WHERE task_key=$1 AND state='leased' AND lease_owner=$2 AND lease_generation=$3 AND lease_until>now() RETURNING task_key),
 event AS(INSERT INTO llm_work_events(task_key,state,reason) SELECT task_key,$4,$5 FROM changed)
 SELECT EXISTS(SELECT 1 FROM changed)`, l.Task.Key, l.Owner, l.Generation, state, reason, retryAfter).Scan(&changed)
	if err != nil {
		return err
	}
	if !changed {
		return ErrLease
	}
	return nil
}

func (s *Store) Heartbeat(ctx context.Context, l Lease) error {
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `UPDATE llm_work_tasks SET lease_until=now()+make_interval(secs=>$4)
 WHERE task_key=$1 AND state='leased' AND lease_owner=$2 AND lease_generation=$3 AND lease_until>now()`, l.Task.Key, l.Owner, l.Generation, s.cfg.LeaseDuration.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLease
	}
	return nil
}

func (s *Store) MarkProgress(ctx context.Context, l Lease) error {
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `UPDATE llm_work_tasks SET priority=100
 WHERE task_key=$1 AND state='leased' AND lease_owner=$2 AND lease_generation=$3 AND lease_until>now()`, l.Task.Key, l.Owner, l.Generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLease
	}
	return nil
}

// Apply commits a narrow source adapter and its completion marker together.
// The adapter owns current-source/target/receipt checks inside this transaction.
func (s *Store) Apply(ctx context.Context, l Lease, reason string, apply func(pgx.Tx) error) error {
	pool, err := s.pool.Get()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var owned bool
	err = tx.QueryRow(ctx, `SELECT true FROM llm_work_tasks WHERE task_key=$1 AND state='leased' AND lease_owner=$2 AND lease_generation=$3 AND lease_until>now() FOR UPDATE`, l.Task.Key, l.Owner, l.Generation).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLease
	}
	if err != nil {
		return err
	}
	if apply != nil {
		if err = apply(tx); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE llm_work_tasks SET state='completed',reason=$2,completed_at=clock_timestamp(),lease_owner=NULL,lease_until=NULL
 WHERE task_key=$1 AND state='leased' AND lease_owner=$3 AND lease_generation=$4 AND lease_until>clock_timestamp()`, l.Task.Key, reason, l.Owner, l.Generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLease
	}
	if _, err = tx.Exec(ctx, `INSERT INTO llm_work_events(task_key,state,reason)VALUES($1,'completed',$2)`, l.Task.Key, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
