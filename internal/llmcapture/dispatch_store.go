package llmcapture

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresDispatchController struct {
	store   *PostgresStore
	enabled bool
}

// NewPostgresDispatchController is an explicit opt-in. A disabled controller
// does not change legacy clients or install a database schema at runtime.
func NewPostgresDispatchController(store *PostgresStore, enabled bool) *PostgresDispatchController {
	return &PostgresDispatchController{store: store, enabled: enabled}
}

func (d *PostgresDispatchController) Enabled() bool {
	return d != nil && d.enabled && d.store != nil && d.store.pool != nil
}

func (d *PostgresDispatchController) pool() (*pgxpool.Pool, error) {
	if !d.Enabled() {
		return nil, fmt.Errorf("%w: dispatch controller disabled", ErrCaptureUnavailable)
	}
	return d.store.pool.Get()
}

func validateDispatchRequest(req DispatchRequest) error {
	if len(req.CaptureKey) != sha256.Size || len(req.InfoHash) != 20 ||
		req.Task == TaskJunkPurge || validateTaskSource(req.Task, req.CandidateSource) != nil {
		return fmt.Errorf("%w: invalid dispatch binding", ErrCaptureUnavailable)
	}
	if req.Case != nil && (len(req.Case.TaskKey) != sha256.Size ||
		len(req.Case.SourceDigest) != sha256.Size || len(req.Case.PolicyDigest) != sha256.Size ||
		req.Case.LeaseOwner == "" || len(req.Case.LeaseOwner) > 64 || req.Case.LeaseGeneration <= 0 || req.Case.SourceRecheck == nil) {
		return ErrDispatchLease
	}
	return nil
}

func dispatchScope(task Task) string {
	switch task {
	case TaskMatcherExtract, TaskMatcherRerank, TaskMatcherEmbedding:
		return "matcher"
	case TaskClassifierType:
		return "classifier_type"
	case TaskContentFilter:
		return "contentfilter"
	}
	return ""
}

func cloneCase(f *CaseFence) *CaseFence {
	if f == nil {
		return nil
	}
	v := *f
	v.TaskKey = append([]byte(nil), f.TaskKey...)
	v.SourceDigest = append([]byte(nil), f.SourceDigest...)
	v.PolicyDigest = append([]byte(nil), f.PolicyDigest...)
	return &v
}

func checkDispatchAdmission(ctx context.Context, tx pgx.Tx, req DispatchRequest) error {
	var admitted bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (`+resultPublicAdmissionSQL+`
 AND c.task=$2 AND COALESCE(c.candidate_source,'')=$3 AND a.info_hash=$4
)`, req.CaptureKey, string(req.Task), string(req.CandidateSource), req.InfoHash).Scan(&admitted)
	if err != nil {
		return err
	}
	if !admitted {
		return ErrPrivacyBlocked
	}
	return nil
}

func checkCaseFence(ctx context.Context, tx pgx.Tx, fence *CaseFence) error {
	if fence == nil {
		return nil
	}
	if fence.SourceRecheck == nil {
		return ErrDispatchLease
	}
	var owned int
	err := tx.QueryRow(ctx, `SELECT 1 FROM llm_work_tasks
WHERE task_key=$1 AND state='leased' AND lease_owner=$2 AND lease_generation=$3
 AND lease_until>clock_timestamp() AND source_digest=$4 AND policy_digest=$5 FOR SHARE`,
		fence.TaskKey, fence.LeaseOwner, fence.LeaseGeneration, fence.SourceDigest, fence.PolicyDigest).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDispatchLease
	}
	if err != nil {
		return err
	}
	return fence.SourceRecheck(ctx, tx)
}

func (d *PostgresDispatchController) Prepare(ctx context.Context, req DispatchRequest) (DispatchLease, DispatchOutcome, error) {
	var lease DispatchLease
	if err := validateDispatchRequest(req); err != nil {
		return lease, DispatchUnknown, err
	}
	duration := req.LeaseDuration
	if duration == 0 {
		duration = time.Minute
	}
	if duration <= 0 || duration > 15*time.Minute {
		return lease, DispatchUnknown, ErrDispatchLease
	}
	pool, err := d.pool()
	if err != nil {
		return lease, DispatchUnknown, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return lease, DispatchUnknown, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := checkDispatchAdmission(ctx, tx, req); err != nil {
		return lease, DispatchUnknown, err
	}
	// Serialize even the initially absent digest fence without tying its
	// lifetime to a raw capture row. The row itself is never TTL-deleted.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(encode($1::bytea,'hex'),718432))`, req.CaptureKey); err != nil {
		return lease, DispatchUnknown, err
	}
	var state string
	var until *time.Time
	var retryAt *time.Time
	var existingTask string
	var existingSource string
	var existingTaskKey []byte
	var permanentDigest []byte
	err = tx.QueryRow(ctx, `SELECT state,lease_until,retry_after,task,candidate_source,task_key,response_sha256
FROM llm_capture_dispatch_attempts WHERE capture_key=$1 FOR UPDATE`, req.CaptureKey).
		Scan(&state, &until, &retryAt, &existingTask, &existingSource, &existingTaskKey, &permanentDigest)
	newFence := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !newFence {
		return lease, DispatchUnknown, err
	}
	if err := checkCaseFence(ctx, tx, req.Case); err != nil {
		return lease, DispatchUnknown, err
	}
	if !newFence && (existingTask != string(req.Task) || existingSource != string(req.CandidateSource) ||
		(len(existingTaskKey) > 0 && req.Case == nil) ||
		(req.Case != nil && len(existingTaskKey) > 0 && !bytes.Equal(existingTaskKey, req.Case.TaskKey))) {
		return lease, DispatchUnknown, ErrDispatchLease
	}
	var body []byte
	var digest []byte
	var status int
	var errorClass string
	resultErr := tx.QueryRow(ctx, `SELECT response_body,response_sha256,http_status,error_class
FROM llm_evaluation_capture_results WHERE capture_key=$1 AND octet_length(response_body)<=$2`, req.CaptureKey, MaxResultBodyBytes).
		Scan(&body, &digest, &status, &errorClass)
	if resultErr != nil && !errors.Is(resultErr, pgx.ErrNoRows) {
		return lease, DispatchUnknown, resultErr
	}
	if resultErr == nil && status > 0 {
		actual := sha256.Sum256(body)
		if !bytes.Equal(digest, actual[:]) || (len(permanentDigest) > 0 && !bytes.Equal(permanentDigest, digest)) {
			return lease, DispatchUnknown, ErrCaptureUnavailable
		}
		var taskKey []byte
		if req.Case != nil {
			taskKey = req.Case.TaskKey
		}
		_, err = tx.Exec(ctx, `INSERT INTO llm_capture_dispatch_attempts
(capture_key,task,candidate_source,task_key,state,response_sha256,http_status,error_class,result_at)
VALUES ($1,$2,$3,$4,'result',$5,$6,$7,clock_timestamp())
ON CONFLICT(capture_key) DO UPDATE SET state='result',response_sha256=EXCLUDED.response_sha256,
 http_status=EXCLUDED.http_status,error_class=EXCLUDED.error_class,result_at=COALESCE(llm_capture_dispatch_attempts.result_at,EXCLUDED.result_at),
 task_key=COALESCE(llm_capture_dispatch_attempts.task_key,EXCLUDED.task_key),updated_at=clock_timestamp()
WHERE llm_capture_dispatch_attempts.response_sha256 IS NULL OR llm_capture_dispatch_attempts.response_sha256=EXCLUDED.response_sha256`,
			req.CaptureKey, string(req.Task), string(req.CandidateSource), taskKey, digest, status, errorClass)
		if err != nil {
			return lease, DispatchUnknown, err
		}
		if err := tx.Commit(ctx); err != nil {
			return lease, DispatchUnknown, err
		}
		return lease, DispatchReplay, nil
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return lease, DispatchUnknown, err
	}
	if !newFence && state == "intent" && until != nil && until.After(now) {
		return lease, DispatchBusy, ErrDispatchBusy
	}
	if (!newFence && (state == "intent" || state == "unknown" || state == "result")) ||
		(newFence && (!req.FreshCapture || resultErr == nil)) {
		var taskKey []byte
		if req.Case != nil {
			taskKey = req.Case.TaskKey
		}
		_, err = tx.Exec(ctx, `INSERT INTO llm_capture_dispatch_attempts(capture_key,task,candidate_source,task_key,state,reason)
VALUES ($1,$2,$3,$4,'unknown','legacy_or_uncertain_dispatch')
ON CONFLICT(capture_key) DO UPDATE SET state='unknown',reason='uncertain_or_expired_result',
 task_key=COALESCE(llm_capture_dispatch_attempts.task_key,EXCLUDED.task_key),updated_at=clock_timestamp()`,
			req.CaptureKey, string(req.Task), string(req.CandidateSource), taskKey)
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			return lease, DispatchUnknown, err
		}
		return lease, DispatchUnknown, ErrDispatchUnknown
	}
	if !newFence && state == "no_dispatch" && retryAt != nil && retryAt.After(now) {
		return lease, DispatchBusy, &DispatchDeferredError{Reason: "retry_not_due", RetryAfterUTC: retryAt.UTC()}
	}
	if !newFence && until != nil && until.After(now) {
		return lease, DispatchBusy, ErrDispatchBusy
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return lease, DispatchUnknown, err
	}
	lease = DispatchLease{CaptureKey: append([]byte(nil), req.CaptureKey...), Owner: hex.EncodeToString(token[:]), Case: cloneCase(req.Case)}
	var taskKey []byte
	if req.Case != nil {
		taskKey = req.Case.TaskKey
	}
	err = tx.QueryRow(ctx, `INSERT INTO llm_capture_dispatch_attempts
(capture_key,task,candidate_source,task_key,state,lease_owner,lease_generation,lease_until)
VALUES ($1,$2,$3,$4,'prepared',$5,1,clock_timestamp()+$6::interval)
ON CONFLICT(capture_key) DO UPDATE SET state='prepared',lease_owner=EXCLUDED.lease_owner,
 lease_generation=llm_capture_dispatch_attempts.lease_generation+1,lease_until=EXCLUDED.lease_until,
 task_key=COALESCE(llm_capture_dispatch_attempts.task_key,EXCLUDED.task_key),retry_after=NULL,reason='',updated_at=clock_timestamp()
RETURNING lease_generation`, req.CaptureKey, string(req.Task), string(req.CandidateSource), taskKey, lease.Owner, duration.String()).Scan(&lease.Generation)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return DispatchLease{}, DispatchUnknown, err
	}
	return lease, DispatchPrepared, nil
}

// reserveDispatchBudgetSQL preserves the existing scopes, UTC-day rollover,
// monthly counter and positive finite limits. Reservations are never refunded.
const reserveDispatchBudgetSQL = `
INSERT INTO llm_request_budgets(scope,month_start,day_start,daily_calls,monthly_calls)
SELECT $3::text,date_trunc('month',now() AT TIME ZONE 'UTC')::date,
 (now() AT TIME ZONE 'UTC')::date,1,1
WHERE $1::integer>0 AND $2::integer>0 AND $3::text IN ('matcher','classifier_type','contentfilter','junkpurge')
ON CONFLICT(scope,month_start) DO UPDATE SET day_start=EXCLUDED.day_start,
 daily_calls=CASE WHEN llm_request_budgets.day_start=EXCLUDED.day_start THEN llm_request_budgets.daily_calls+1 ELSE 1 END,
 monthly_calls=llm_request_budgets.monthly_calls+1
WHERE llm_request_budgets.monthly_calls<$2
 AND (CASE WHEN llm_request_budgets.day_start=EXCLUDED.day_start THEN llm_request_budgets.daily_calls ELSE 0 END)<$1
RETURNING monthly_calls`

func (d *PostgresDispatchController) Reserve(ctx context.Context, lease DispatchLease, scope string, daily, monthly int) (bool, error) {
	pool, err := d.pool()
	if err != nil {
		return false, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var task Task
	var reusable bool
	var linkedTask []byte
	err = tx.QueryRow(ctx, `SELECT task,COALESCE(reserved_day=(clock_timestamp() AT TIME ZONE 'UTC')::date AND budget_scope=$4,false),task_key
FROM llm_capture_dispatch_attempts WHERE capture_key=$1 AND lease_owner=$2 AND lease_generation=$3
 AND lease_until>clock_timestamp() AND state IN ('prepared','admitted') FOR UPDATE`, lease.CaptureKey, lease.Owner, lease.Generation, scope).
		Scan(&task, &reusable, &linkedTask)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrDispatchLease
	}
	if err != nil {
		return false, err
	}
	if dispatchScope(task) != scope {
		return false, ErrDispatchLease
	}
	if len(linkedTask) > 0 && (lease.Case == nil || !bytes.Equal(linkedTask, lease.Case.TaskKey)) {
		return false, ErrDispatchLease
	}
	if daily <= 0 || monthly <= 0 {
		return false, nil
	}
	if err := checkCaseFence(ctx, tx, lease.Case); err != nil {
		return false, err
	}
	if !reusable {
		var used int
		err = tx.QueryRow(ctx, reserveDispatchBudgetSQL, daily, monthly, scope).Scan(&used)
		if errors.Is(err, pgx.ErrNoRows) {
			var monthlyExhausted bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM llm_request_budgets
WHERE scope=$1 AND month_start=date_trunc('month',clock_timestamp() AT TIME ZONE 'UTC')::date AND monthly_calls>=$2)`, scope, monthly).Scan(&monthlyExhausted)
			if err != nil {
				return false, err
			}
			reason, period := "daily_budget", "day"
			if monthlyExhausted {
				reason, period = "monthly_budget", "month"
			}
			var retry time.Time
			err = tx.QueryRow(ctx, `UPDATE llm_capture_dispatch_attempts SET state='no_dispatch',reason=$2,
lease_until=NULL,retry_after=(date_trunc($3,clock_timestamp() AT TIME ZONE 'UTC')+
 CASE WHEN $3='month' THEN interval '1 month' ELSE interval '1 day' END) AT TIME ZONE 'UTC',updated_at=clock_timestamp()
WHERE capture_key=$1 RETURNING retry_after`, lease.CaptureKey, reason, period).Scan(&retry)
			if err == nil {
				err = tx.Commit(ctx)
			}
			if err != nil {
				return false, err
			}
			return false, &DispatchDeferredError{Reason: reason, RetryAfterUTC: retry.UTC()}
		}
		if err != nil {
			return false, err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE llm_capture_dispatch_attempts SET state='admitted',budget_scope=$2,
reserved_day=(clock_timestamp() AT TIME ZONE 'UTC')::date,
reserved_month=date_trunc('month',clock_timestamp() AT TIME ZONE 'UTC')::date,updated_at=clock_timestamp() WHERE capture_key=$1`, lease.CaptureKey, scope)
	if err == nil {
		err = tx.Commit(ctx)
	}
	return err == nil, err
}

func caseArgs(f *CaseFence) []any {
	if f == nil {
		return []any{nil, "", int64(0), nil, nil}
	}
	return []any{f.TaskKey, f.LeaseOwner, f.LeaseGeneration, f.SourceDigest, f.PolicyDigest}
}

func (d *PostgresDispatchController) BeginDispatch(ctx context.Context, lease DispatchLease) error {
	pool, err := d.pool()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Match Reserve's lock order: dispatch row, owned task, current source.
	var locked int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM llm_capture_dispatch_attempts
WHERE capture_key=$1 AND lease_owner=$2 AND lease_generation=$3 FOR UPDATE`,
		lease.CaptureKey, lease.Owner, lease.Generation).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDispatchLease
		}
		return err
	}
	if err := checkCaseFence(ctx, tx, lease.Case); err != nil {
		return err
	}
	args := []any{lease.CaptureKey, lease.Owner, lease.Generation}
	args = append(args, caseArgs(lease.Case)...)
	tag, err := tx.Exec(ctx, `UPDATE llm_capture_dispatch_attempts d SET state='intent',dispatch_intent_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE d.capture_key=$1 AND d.lease_owner=$2 AND d.lease_generation=$3 AND d.lease_until>clock_timestamp()
 AND d.state='admitted' AND d.reserved_day=(clock_timestamp() AT TIME ZONE 'UTC')::date
 AND EXISTS (`+resultPublicAdmissionSQL+`)
 AND ((d.task_key IS NULL AND $4::bytea IS NULL) OR (d.task_key=$4 AND EXISTS(SELECT 1 FROM llm_work_tasks w
  WHERE w.task_key=$4 AND w.state='leased' AND w.lease_owner=$5 AND w.lease_generation=$6
   AND w.lease_until>clock_timestamp() AND w.source_digest=$7 AND w.policy_digest=$8)))`, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrDispatchLease
	}
	return tx.Commit(ctx)
}

func (d *PostgresDispatchController) DeferNoDispatch(ctx context.Context, lease DispatchLease, reason string, retryAt time.Time) error {
	if len(reason) > 64 {
		return ErrDispatchLease
	}
	pool, err := d.pool()
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `UPDATE llm_capture_dispatch_attempts SET state='no_dispatch',reason=$4,retry_after=$5,lease_until=NULL,updated_at=clock_timestamp()
WHERE capture_key=$1 AND lease_owner=$2 AND lease_generation=$3 AND lease_until>clock_timestamp()
 AND state IN ('prepared','admitted')`, lease.CaptureKey, lease.Owner, lease.Generation, reason, retryAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrDispatchLease
	}
	return nil
}

// ObserveResult reconciles a late exact immutable receipt even after its lease
// expired. It never accepts a replacement response or creates HTTP evidence.
func (d *PostgresDispatchController) ObserveResult(ctx context.Context, lease DispatchLease, receipt ResultReceipt) error {
	if !bytes.Equal(lease.CaptureKey, receipt.CaptureKey) || len(receipt.ResponseSHA256) != sha256.Size {
		return ErrCaptureUnavailable
	}
	pool, err := d.pool()
	if err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, `UPDATE llm_capture_dispatch_attempts d
SET state=CASE WHEN r.http_status=0 THEN 'unknown' ELSE 'result' END,
 response_sha256=r.response_sha256,http_status=r.http_status,error_class=r.error_class,
 result_at=clock_timestamp(),updated_at=clock_timestamp()
FROM llm_evaluation_capture_results r
WHERE d.capture_key=$1 AND d.lease_owner=$2 AND d.lease_generation=$3
 AND d.state IN ('intent','unknown','result') AND r.capture_key=d.capture_key
 AND r.response_sha256=$4 AND r.http_status=$5 AND r.error_class=$6
 AND octet_length(r.response_body)<=$7
 AND (d.response_sha256 IS NULL OR d.response_sha256=r.response_sha256)
 AND EXISTS (`+resultPublicAdmissionSQL+`)`, lease.CaptureKey, lease.Owner, lease.Generation,
		receipt.ResponseSHA256, receipt.StatusCode, receipt.ErrorClass, MaxResultBodyBytes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrCaptureUnavailable
	}
	return nil
}

func (d *PostgresDispatchController) Replay(ctx context.Context, req DispatchRequest) (HTTPReplay, error) {
	var replay HTTPReplay
	if err := validateDispatchRequest(req); err != nil {
		return replay, err
	}
	pool, err := d.pool()
	if err != nil {
		return replay, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return replay, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := checkDispatchAdmission(ctx, tx, req); err != nil {
		return replay, err
	}
	var locked int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM llm_capture_dispatch_attempts WHERE capture_key=$1 FOR SHARE`, req.CaptureKey).Scan(&locked); err != nil {
		return replay, ErrDispatchUnknown
	}
	if err := checkCaseFence(ctx, tx, req.Case); err != nil {
		return replay, err
	}
	var digest []byte
	var taskKey []byte
	if req.Case != nil {
		taskKey = req.Case.TaskKey
	}
	err = tx.QueryRow(ctx, `SELECT r.response_body,r.response_sha256,r.http_status,r.error_class
FROM llm_evaluation_capture_results r JOIN llm_capture_dispatch_attempts d USING(capture_key)
WHERE r.capture_key=$1 AND d.state='result' AND d.response_sha256=r.response_sha256
 AND d.http_status=r.http_status AND d.error_class=r.error_class
 AND r.http_status>0 AND octet_length(r.response_body)<=$2
 AND d.task=$3 AND d.candidate_source=$4
 AND ((d.task_key IS NULL AND $5::bytea IS NULL) OR d.task_key=$5)`, req.CaptureKey, MaxResultBodyBytes,
		string(req.Task), string(req.CandidateSource), taskKey).
		Scan(&replay.Result.Body, &digest, &replay.Result.StatusCode, &replay.Result.ErrorClass)
	if err != nil {
		return HTTPReplay{}, fmt.Errorf("%w: retained first response unavailable", ErrDispatchUnknown)
	}
	actual := sha256.Sum256(replay.Result.Body)
	if !bytes.Equal(digest, actual[:]) {
		return HTTPReplay{}, ErrCaptureUnavailable
	}
	// This is the original first response, explicitly a cache replay rather
	// than another HTTP observation. Existing idempotent decision contracts
	// already distinguish that original authority from cohort membership.
	replay.Receipt = ResultReceipt{CaptureKey: append([]byte(nil), req.CaptureKey...),
		ResponseSHA256: append([]byte(nil), digest...), FirstObservation: true,
		FromCache: true, StatusCode: replay.Result.StatusCode, ErrorClass: replay.Result.ErrorClass}
	if err := tx.Commit(ctx); err != nil {
		return HTTPReplay{}, err
	}
	return replay, nil
}

func (d *PostgresDispatchController) TaskRecovery(ctx context.Context, taskKey []byte) ([]DispatchRecovery, error) {
	if len(taskKey) != sha256.Size {
		return nil, ErrDispatchLease
	}
	pool, err := d.pool()
	if err != nil {
		return nil, err
	}
	// A crash after immutable result persistence but before ObserveResult is
	// recoverable from that exact first row. This does not dispatch or refund.
	if _, err := pool.Exec(ctx, `UPDATE llm_capture_dispatch_attempts d SET state='result',
response_sha256=r.response_sha256,http_status=r.http_status,error_class=r.error_class,
result_at=COALESCE(d.result_at,r.observed_at),updated_at=clock_timestamp()
FROM llm_evaluation_capture_results r JOIN llm_evaluation_captures c USING(capture_key)
WHERE d.task_key=$1 AND d.capture_key=r.capture_key AND d.state IN ('intent','unknown')
 AND c.expires_at>clock_timestamp() AND c.task=d.task AND COALESCE(c.candidate_source,'')=d.candidate_source
 AND r.http_status>0 AND octet_length(r.response_body)<=$2
 AND (d.response_sha256 IS NULL OR d.response_sha256=r.response_sha256)`, taskKey, MaxResultBodyBytes); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT d.capture_key,d.state,
 EXISTS(SELECT 1 FROM llm_evaluation_capture_results r JOIN llm_evaluation_captures c USING(capture_key)
  WHERE r.capture_key=d.capture_key AND c.expires_at>clock_timestamp()
   AND r.response_sha256=d.response_sha256 AND r.http_status>0
   AND octet_length(r.response_body)<=$2) AS replayable,
 d.state IN ('prepared','no_dispatch','admitted') AS safe_to_retry
FROM llm_capture_dispatch_attempts d WHERE task_key=$1 ORDER BY capture_key LIMIT 257`, taskKey, MaxResultBodyBytes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DispatchRecovery
	for rows.Next() {
		var item DispatchRecovery
		if err := rows.Scan(&item.CaptureKey, &item.State, &item.Replayable, &item.SafeToRetry); err != nil {
			return nil, err
		}
		out = append(out, item)
		if len(out) > 256 {
			return nil, fmt.Errorf("%w: request recovery exceeds bounded case limit", ErrCaptureUnavailable)
		}
	}
	return out, rows.Err()
}

var _ DispatchControl = (*PostgresDispatchController)(nil)
