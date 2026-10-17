package processors

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	llmwork "github.com/spencercnorton/bitagent/internal/legacyresults"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/protocol"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

// Reprocess uses the ordinary core queue first. It never clears a match, removes
// a restriction, rewrites an old receipt, or lets a service schedule its own work.
func (s *Store) Reprocess(ctx context.Context, origin string, w v1.ReprocessWrite) (v1.ReprocessStatus, error) {
	if !jobIdentity.MatchString(origin) || !jobIdentity.MatchString(w.RequestID) ||
		!jobIdentity.MatchString(w.ExpectedInputRevision) || !jobIdentity.MatchString(w.ExpectedPipelineRevision) || !admissionVersion.MatchString(w.Reason) {
		return v1.ReprocessStatus{}, ErrControl
	}
	p, err := s.pool.Get()
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	fingerprint := digest([]any{origin, w})
	// An idempotent retry can still reconcile after its origin job was pruned.
	var prior string
	err = p.QueryRow(ctx, `SELECT request_digest FROM processor_reprocess_requests WHERE request_id=$1`, w.RequestID).Scan(&prior)
	if err == nil {
		if prior != fingerprint {
			return v1.ReprocessStatus{}, ErrConflict
		}
		return s.Reprocessing(ctx, w.RequestID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return v1.ReprocessStatus{}, err
	}
	export, err := s.ExportJob(ctx, origin)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	if export.Job.InputRevision != w.ExpectedInputRevision {
		return v1.ReprocessStatus{}, ErrConflict
	}
	if export.Job.State == "queued" || export.Job.State == "deferred" || export.Job.State == "leased" {
		return v1.ReprocessStatus{}, ErrConflict
	}
	archive, err := json.Marshal(export)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR UPDATE`); err != nil {
		return v1.ReprocessStatus{}, err
	}
	// Serialize duplicate requests and capacity with all other core admission.
	err = tx.QueryRow(ctx, `SELECT request_digest FROM processor_reprocess_requests WHERE request_id=$1`, w.RequestID).Scan(&prior)
	if err == nil {
		if prior != fingerprint {
			return v1.ReprocessStatus{}, ErrConflict
		}
		rollback(tx)
		return s.Reprocessing(ctx, w.RequestID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return v1.ReprocessStatus{}, err
	}
	plan, _, err := readPlan(ctx, tx, s.policy)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	if digest(plan) != w.ExpectedPipelineRevision {
		return v1.ReprocessStatus{}, ErrConflict
	}
	var total, pending int
	err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE state IN('waiting_core','ready','queued')) FROM processor_reprocess_requests`).Scan(&total, &pending)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	if total >= min(s.cfg.RetainedJobs, 1000) || pending >= min(s.cfg.QueueCapacity, 64) {
		return v1.ReprocessStatus{}, ErrCapacity
	}
	hash, err := hex.DecodeString(export.Input.InfoHash)
	if err != nil || len(hash) != 20 {
		return v1.ReprocessStatus{}, ErrControl
	}
	var busy bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM processor_jobs WHERE info_hash=$1 AND state IN('queued','deferred','leased')) OR
 EXISTS(SELECT 1 FROM processor_reprocess_requests WHERE info_hash=$1 AND state IN('waiting_core','ready','queued'))`, hash).Scan(&busy)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	if busy {
		return v1.ReprocessStatus{}, ErrConflict
	}
	src, raw, err := readSource(ctx, tx, hash, true)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	if !src.admitted() || s.names != nil && !s.names.Evaluate(src.torrent(export.Input.InfoHash).InfoHash, src.Name).Eligible {
		return v1.ReprocessStatus{}, ErrStale
	}
	var before []byte
	err = tx.QueryRow(ctx, `SELECT jsonb_build_object('source',$2::jsonb,
 'application',(SELECT to_jsonb(a) FROM processor_applications a WHERE info_hash=$1),
 'restriction',(SELECT to_jsonb(e) FROM processor_exclusions e WHERE info_hash=$1))`, hash, string(raw)).Scan(&before)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	if len(before) > 2<<20 {
		return v1.ReprocessStatus{}, errExportLimit
	}
	id, err := protocol.ParseID(export.Input.InfoHash)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	job, err := processor.NewQueueJob(processor.MessageParams{ReprocessRequestID: w.RequestID, InfoHashes: []protocol.ID{id}})
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	var coreJob string
	err = tx.QueryRow(ctx, `INSERT INTO queue_jobs(fingerprint,queue,status,payload,retries,max_retries,run_after,deadline,archival_duration,created_at,priority)
 VALUES($1,$2,'pending',$3,0,$4,clock_timestamp(),clock_timestamp()+interval '1 hour',interval '7 days',clock_timestamp(),0) RETURNING id`, job.Fingerprint, job.Queue, job.Payload, job.MaxRetries).Scan(&coreJob)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO processor_reprocess_requests(request_id,request_digest,origin_job_id,info_hash,expected_pipeline_revision,reason,state,core_job_id,origin_export,before_snapshot)
 VALUES($1,$2,$3,$4,$5,$6,'waiting_core',$7,$8,$9)`, w.RequestID, fingerprint, origin, hash, w.ExpectedPipelineRevision, w.Reason, coreJob, string(archive), string(before))
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return v1.ReprocessStatus{}, err
	}
	return s.Reprocessing(ctx, w.RequestID)
}

func (s *Store) Reprocessing(ctx context.Context, id string) (v1.ReprocessStatus, error) {
	out := v1.ReprocessStatus{Contract: v1.ReprocessingContract}
	if !jobIdentity.MatchString(id) {
		return out, ErrControl
	}
	p, err := s.pool.Get()
	if err != nil {
		return out, err
	}
	return readReprocessing(ctx, p, id)
}

func readReprocessing(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (v1.ReprocessStatus, error) {
	out := v1.ReprocessStatus{Contract: v1.ReprocessingContract}
	var created, updated time.Time
	err := q.QueryRow(ctx, `SELECT request_id,origin_job_id,expected_pipeline_revision,reason,state,state_reason,core_job_id,result_job_id,created_at,updated_at
 FROM processor_reprocess_requests WHERE request_id=$1`, id).Scan(&out.RequestID, &out.OriginJobID, &out.ExpectedPipelineRevision, &out.Reason, &out.State, &out.StateReason, &out.CoreJobID, &out.ResultJobID, &created, &updated)
	out.CreatedAt, out.UpdatedAt = stamp(created), stamp(updated)
	return out, err
}

func (s *Store) ExportReprocessing(ctx context.Context, id string) (v1.ReprocessingAudit, error) {
	out := v1.ReprocessingAudit{Contract: v1.ReprocessingContract}
	if !jobIdentity.MatchString(id) {
		return out, ErrControl
	}
	p, err := s.pool.Get()
	if err != nil {
		return out, err
	}
	tx, err := p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer rollback(tx)
	out.Request, err = readReprocessing(ctx, tx, id)
	if err != nil {
		return out, err
	}
	var archive []byte
	err = tx.QueryRow(ctx, `SELECT origin_export,before_snapshot,core_receipt FROM processor_reprocess_requests WHERE request_id=$1`, id).Scan(&archive, &out.BeforeSnapshot, &out.CoreReceipt)
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(archive, &out.OriginExport); err != nil {
		return out, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if len(raw) > v1.MaxReprocessingExportBody {
		return out, errExportLimit
	}
	return out, nil
}

// settleReprocessing never infers normal matching from a processed queue status.
// It requires the receipt correlated with this request, then rechecks its source,
// current authority and exact pipeline before admitting optional work.
func (s *Store) settleReprocessing(ctx context.Context, tx pgx.Tx, plan Plan, draining bool) error {
	_, err := tx.Exec(ctx, `UPDATE processor_reprocess_requests r SET state='settled',state_reason=j.state||':'||j.outcome,updated_at=clock_timestamp()
 FROM processor_jobs j WHERE r.result_job_id=j.job_id AND r.state='queued' AND j.state IN('completed','failed','stale','canceled')`)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='stale',state_reason='pipeline_changed',updated_at=clock_timestamp()
 WHERE state='ready' AND expected_pipeline_revision<>$1`, digest(plan))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='deferred',state_reason='optional_admission_expired',updated_at=clock_timestamp()
 WHERE state='ready' AND created_at<clock_timestamp()-interval '1 hour'`)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT r.request_id,r.info_hash,r.expected_pipeline_revision,r.core_receipt,COALESCE(q.status::text,'missing'),r.created_at<clock_timestamp()-interval '1 hour'
 FROM processor_reprocess_requests r LEFT JOIN queue_jobs q ON q.id=r.core_job_id
 WHERE r.state='waiting_core' AND (q.status IN('processed','failed') OR q.id IS NULL OR r.created_at<clock_timestamp()-interval '1 hour')
 ORDER BY r.created_at LIMIT 16 FOR UPDATE OF r`)
	if err != nil {
		return err
	}
	type pending struct {
		id       string
		hash     []byte
		pipeline string
		receipt  []byte
		status   string
		expired  bool
	}
	items := []pending{}
	for rows.Next() {
		var x pending
		if err = rows.Scan(&x.id, &x.hash, &x.pipeline, &x.receipt, &x.status, &x.expired); err != nil {
			rows.Close()
			return err
		}
		items = append(items, x)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, x := range items {
		state, reason := "held", "core_receipt_unavailable"
		switch {
		case x.expired:
			state, reason = "deferred", "core_processing_expired"
		case x.status == "failed":
			state, reason = "failed", "core_processing_failed"
		case x.status != "processed":
			state, reason = "deferred", "core_processing_unavailable_or_expired"
		case x.pipeline != digest(plan):
			state, reason = "stale", "pipeline_changed"
		case len(x.receipt) > 0:
			var receipt struct {
				Processing string `json:"processing_outcome"`
				Digest     string `json:"source_digest"`
				Policy     string `json:"core_policy"`
			}
			if err = json.Unmarshal(x.receipt, &receipt); err != nil {
				return err
			}
			src, _, readErr := readSource(ctx, tx, x.hash, false)
			if errors.Is(readErr, pgx.ErrNoRows) {
				state, reason = "held", "source_unavailable"
			} else if readErr != nil {
				return readErr
			} else if !src.admitted() || s.names != nil && !s.names.Evaluate(src.torrent(hex.EncodeToString(x.hash)).InfoHash, src.Name).Eligible {
				state, reason = "held", "current_authority_denied"
			} else if receipt.Policy != s.policy || receipt.Digest != `\x`+hex.EncodeToString(llmwork.SourceDigest(src.torrent(hex.EncodeToString(x.hash)))) {
				state, reason = "stale", "core_receipt_source_changed"
			} else if receipt.Processing != "completed" {
				state, reason = "deferred", "normal_processing_incomplete"
			} else if len(plan.Steps) == 0 {
				state, reason = "core_only", "no_active_processors"
			} else if draining || !admittedByPolicy(x.hash, plan.Admission) {
				state, reason = "held", "admission_policy_denied"
			} else {
				state, reason = "ready", "normal_processing_completed"
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state=$2,state_reason=$3,updated_at=clock_timestamp() WHERE request_id=$1`, x.id, state, reason); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CancelReprocessing(ctx context.Context, id string) (v1.ReprocessStatus, error) {
	if !jobIdentity.MatchString(id) {
		return v1.ReprocessStatus{}, ErrControl
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR UPDATE`); err != nil {
		return v1.ReprocessStatus{}, err
	}
	var job *string
	err = tx.QueryRow(ctx, `SELECT result_job_id FROM processor_reprocess_requests WHERE request_id=$1 FOR UPDATE`, id).Scan(&job)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	if job != nil {
		_, err = tx.Exec(ctx, `WITH canceled AS (UPDATE processor_jobs SET state='canceled',reason='reprocessing_canceled',lease_until=NULL,finished_at=clock_timestamp()
 WHERE job_id=$1 AND state IN('queued','deferred','leased') RETURNING job_id,lease_generation)
 UPDATE processor_attempts a SET finished_at=clock_timestamp(),outcome='unknown',reason='reprocessing_canceled'
 FROM canceled j WHERE a.job_id=j.job_id AND a.lease_generation=j.lease_generation AND a.finished_at IS NULL`, *job)
		if err != nil {
			return v1.ReprocessStatus{}, err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE processor_reprocess_requests SET state='canceled',state_reason='operator_canceled',updated_at=clock_timestamp()
 WHERE request_id=$1 AND state IN('waiting_core','ready','queued')`, id)
	if err != nil {
		return v1.ReprocessStatus{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return v1.ReprocessStatus{}, err
	}
	return s.Reprocessing(ctx, id)
}
