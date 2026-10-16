package processors

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

type Receipt struct {
	Result     v1.Result      `json:"result"`
	Candidates []v1.Candidate `json:"candidates"`
}
type Lease struct {
	ID                              string
	Hash                            []byte
	InputRevision, PipelineRevision string
	Epoch, Generation               int64
	Input                           v1.Input
	Source                          json.RawMessage
	Plan                            Plan
	Results                         []Receipt
	Step, StepAttempts              int
	AvailableAt                     time.Time
}

// Scan is a bounded, restartable catch-up sweep, independent of core queue_jobs.
// The control row serializes admission across replicas. With no enabled service
// it performs no catalogue scan and makes no network request.
func (s *Store) Scan(ctx context.Context) (int, error) {
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	tx, e := s.begin(ctx)
	if e != nil {
		return 0, e
	}
	defer rollback(tx)
	var after []byte
	if e = tx.QueryRow(ctx, `SELECT scan_after FROM processor_control WHERE singleton FOR UPDATE`).Scan(&after); e != nil {
		return 0, e
	}
	plan, draining, e := readPlan(ctx, tx, s.policy)
	if e != nil {
		return 0, e
	}
	s.active.Store(len(plan.Steps) > 0)
	if len(plan.Steps) == 0 || draining || plan.Admission.BasisPoints == 0 {
		return 0, nil
	}
	e = pruneJobs(ctx, tx, s.cfg.Retention)
	if e != nil {
		return 0, e
	}
	var pending, total int
	if e = tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state IN('queued','deferred','leased')),count(*) FROM processor_jobs`).Scan(&pending, &total); e != nil {
		return 0, e
	}
	capacity := min(s.cfg.ScanBatch, s.cfg.QueueCapacity-pending, s.cfg.RetainedJobs-total)
	if capacity <= 0 {
		s.metrics.admission.WithLabelValues("full").Inc()
		return 0, tx.Commit(ctx)
	}
	rows, e := tx.Query(ctx, `SELECT t.info_hash FROM torrents t WHERE t.info_hash>$1 AND EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=t.info_hash) ORDER BY t.info_hash LIMIT $2`, after, s.cfg.ScanBatch)
	if e != nil {
		return 0, e
	}
	var hashes [][]byte
	for rows.Next() {
		var h []byte
		if e = rows.Scan(&h); e != nil {
			rows.Close()
			return 0, e
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return 0, e
	}
	admitted := 0
	for _, h := range hashes {
		after = h
		if !admittedByPolicy(h, plan.Admission) {
			continue
		}
		src, raw, err := readSource(ctx, tx, h, false)
		if err != nil {
			return 0, err
		}
		if !src.admitted() {
			continue
		}
		if s.names != nil && !s.names.Evaluate(src.torrent(hex.EncodeToString(h)).InfoHash, src.Name).Eligible {
			continue
		}
		revision, pipeline := digest(raw), digest(plan)
		var already bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM processor_applications WHERE info_hash=$1 AND applied_revision=$2 AND pipeline_revision=$3)`, h, revision, pipeline).Scan(&already)
		if err != nil {
			return 0, err
		}
		if already {
			continue
		}
		input := src.input(h, s.policy, s.noiseV2)
		ib, _ := json.Marshal(input)
		pb, _ := json.Marshal(plan)
		if len(ib) > 100000 {
			s.metrics.admission.WithLabelValues("oversize").Inc()
			continue
		}
		id := digest([]any{hex.EncodeToString(h), revision, pipeline})
		tag, err := tx.Exec(ctx, `INSERT INTO processor_jobs(job_id,info_hash,input_revision,pipeline_revision,epoch,input,source,plan) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, id, h, revision, pipeline, plan.Epoch, string(ib), string(raw), string(pb))
		if err != nil {
			return 0, err
		}
		admitted += int(tag.RowsAffected())
		if admitted >= capacity {
			break
		}
	}
	if len(hashes) == 0 {
		after = []byte{}
	}
	_, e = tx.Exec(ctx, `UPDATE processor_control SET scan_after=$1 WHERE singleton`, after)
	if e != nil {
		return 0, e
	}
	e = tx.Commit(ctx)
	if e == nil {
		s.metrics.admission.WithLabelValues("queued").Add(float64(admitted))
	}
	return admitted, e
}

func (s *Store) Claim(ctx context.Context) (*Lease, error) {
	ctx, c := context.WithTimeout(ctx, time.Second)
	defer c()
	tx, e := s.begin(ctx)
	if e != nil {
		return nil, e
	}
	defer rollback(tx)
	var epoch int64
	if e = tx.QueryRow(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR UPDATE`).Scan(&epoch); e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `WITH expired AS (
 UPDATE processor_jobs SET state='queued',lease_until=NULL,reason='lease_expired'
 WHERE state='leased' AND lease_until<clock_timestamp() RETURNING job_id,lease_generation)
 UPDATE processor_attempts a SET finished_at=clock_timestamp(),outcome='failed',reason='lease_expired'
 FROM expired j WHERE j.job_id=a.job_id AND j.lease_generation=a.lease_generation AND a.finished_at IS NULL`)
	if e != nil {
		return nil, e
	}
	var busy int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM processor_jobs WHERE state='leased'`).Scan(&busy); e != nil {
		return nil, e
	}
	if busy >= s.cfg.Concurrency {
		return nil, tx.Commit(ctx)
	}
	var l Lease
	var ib, pb, rb []byte
	e = tx.QueryRow(ctx, `UPDATE processor_jobs SET state='leased',lease_generation=lease_generation+1,lease_until=clock_timestamp()+$1::interval
 WHERE job_id=(SELECT job_id FROM processor_jobs WHERE state IN('queued','deferred') AND available_at<=clock_timestamp() AND epoch=$2 ORDER BY available_at,created_at,job_id FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING job_id,info_hash,input_revision,pipeline_revision,epoch,lease_generation,input,source,plan,results,step,step_attempts,available_at`, (s.cfg.RequestTimeout+5*time.Second).String(), epoch).Scan(&l.ID, &l.Hash, &l.InputRevision, &l.PipelineRevision, &l.Epoch, &l.Generation, &ib, &l.Source, &pb, &rb, &l.Step, &l.StepAttempts, &l.AvailableAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if e != nil {
		return nil, e
	}
	for _, x := range []struct {
		raw []byte
		out any
	}{{ib, &l.Input}, {pb, &l.Plan}, {rb, &l.Results}} {
		if e = json.Unmarshal(x.raw, x.out); e != nil {
			return nil, e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return &l, nil
}

func (s *Store) fresh(ctx context.Context, tx pgx.Tx, l Lease, lock bool) (source, error) {
	var epoch int64
	if e := tx.QueryRow(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR SHARE`).Scan(&epoch); e != nil {
		return source{}, e
	}
	if epoch != l.Epoch || l.Plan.CorePolicy != s.policy {
		return source{}, ErrStale
	}
	src, raw, e := readSource(ctx, tx, l.Hash, lock)
	if e != nil {
		return src, e
	}
	if !src.admitted() || digest(raw) != l.InputRevision {
		return src, ErrStale
	}
	if s.names != nil && !s.names.Evaluate(src.torrent(hex.EncodeToString(l.Hash)).InfoHash, src.Name).Eligible {
		return src, ErrStale
	}
	return src, nil
}

// Prepare rechecks authority immediately before disclosure. No transaction is
// held across the service call. Stable step IDs are reused on every retry.
func (s *Store) Prepare(ctx context.Context, l Lease) (v1.Job, error) {
	step := l.Plan.Steps[l.Step]
	tx, e := s.begin(ctx)
	if e != nil {
		return v1.Job{}, e
	}
	defer rollback(tx)
	if _, e = s.fresh(ctx, tx, l, false); e != nil {
		return v1.Job{}, e
	}
	var frozen []byte
	var valid bool
	e = tx.QueryRow(ctx, `SELECT prepared_job,state='leased' AND lease_generation=$2 FROM processor_jobs WHERE job_id=$1 FOR UPDATE`, l.ID, l.Generation).Scan(&frozen, &valid)
	if e != nil {
		return v1.Job{}, e
	}
	if !valid {
		return v1.Job{}, ErrStale
	}
	if len(frozen) > 0 {
		var saved v1.Job
		if e = json.Unmarshal(frozen, &saved); e != nil {
			return saved, e
		}
		saved.Deadline = time.Now().Add(s.cfg.RequestTimeout).UTC().Format(time.RFC3339Nano)
		return saved, saved.Validate()
	}
	in := l.Input
	in.PriorResults = []v1.Result{}
	for _, r := range l.Results {
		in.PriorResults = append(in.PriorResults, r.Result)
	}
	if step.Stage == v1.Match {
		in.Candidates, e = s.candidates(ctx, tx, in, l)
		if e != nil {
			return v1.Job{}, e
		}
	}
	// Candidate content and all previous results are part of the step revision.
	revision := digest([]any{l.InputRevision, in})
	j := v1.Job{Binding: v1.Binding{Contract: v1.Contract, JobID: digest([]any{l.ID, l.Step, revision}), InputRevision: revision, PipelineRevision: l.PipelineRevision, ConfigurationDigest: digest(step.Entry.Configuration), Processor: step.Entry.Manifest.Processor, Stage: step.Stage}, Configuration: step.Entry.Configuration, Input: in, Deadline: time.Now().Add(s.cfg.RequestTimeout).UTC().Format(time.RFC3339Nano)}
	if e = j.Validate(); e != nil {
		return j, e
	}
	raw, _ := json.Marshal(j)
	if len(raw) > v1.MaxBody {
		return j, errors.New("input_body_limit")
	}
	if _, e = tx.Exec(ctx, `UPDATE processor_jobs SET prepared_job=$2 WHERE job_id=$1`, l.ID, string(raw)); e != nil {
		return j, e
	}
	return j, tx.Commit(ctx)
}
func (s *Store) startAttempt(ctx context.Context, l Lease) error {
	tx, e := s.begin(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	tag, e := tx.Exec(ctx, `UPDATE processor_jobs SET attempts=attempts+1,step_attempts=step_attempts+1 WHERE job_id=$1 AND state='leased' AND lease_generation=$2 AND lease_until>clock_timestamp()`, l.ID, l.Generation)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	st := l.Plan.Steps[l.Step]
	wait := max(0, time.Since(l.AvailableAt).Seconds())
	_, e = tx.Exec(ctx, `INSERT INTO processor_attempts(job_id,lease_generation,processor_id,generation,stage,queue_wait_ms) VALUES($1,$2,$3,$4,$5,$6)`, l.ID, l.Generation, st.Entry.ID, st.Entry.ExecutionGeneration, string(st.Stage), wait*1000)
	if e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	gen := strconv.FormatInt(st.Entry.ExecutionGeneration, 10)
	s.metrics.note(st.Entry.ID, gen)
	s.metrics.attempts.WithLabelValues(st.Entry.ID, gen).Inc()
	s.metrics.wait.WithLabelValues(st.Entry.ID, gen, string(st.Stage)).Observe(wait)
	return nil
}

// Finish is a compare-and-swap on the still-valid lease. Terminal results are
// applied once in this transaction, or not at all; duplicate/late responses lose.
func (s *Store) Finish(ctx context.Context, l Lease, j v1.Job, r v1.Result, dispatched bool) error {
	if e := r.Validate(j); e != nil {
		return e
	}
	ctx, c := context.WithTimeout(ctx, 500*time.Millisecond)
	defer c()
	tx, e := s.begin(ctx)
	if e != nil {
		return e
	}
	defer rollback(tx)
	// Registry locks precede job locks everywhere to avoid upgrade deadlocks.
	var epoch int64
	if e = tx.QueryRow(ctx, `SELECT epoch FROM processor_control WHERE singleton FOR SHARE`).Scan(&epoch); e != nil {
		return e
	}
	var valid bool
	e = tx.QueryRow(ctx, `SELECT state='leased' AND lease_generation=$2 AND lease_until>clock_timestamp() FROM processor_jobs WHERE job_id=$1 FOR UPDATE`, l.ID, l.Generation).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrStale
	}
	if epoch != l.Epoch {
		return ErrStale
	}
	if dispatched {
		reason := ""
		if len(r.Reasons) > 0 {
			reason = r.Reasons[0]
			if len(reason) > 128 {
				runes := []rune(reason)
				reason = string(runes[:min(128, len(runes))])
			}
		}
		_, e = tx.Exec(ctx, `UPDATE processor_attempts SET finished_at=clock_timestamp(),outcome=$3,reason=$4,inference_ms=$5 WHERE job_id=$1 AND lease_generation=$2`, l.ID, l.Generation, string(r.Outcome), reason, r.Telemetry.InferenceMS)
		if e != nil {
			return e
		}
	}
	if (r.Outcome == v1.Failed || r.Outcome == v1.Deferred) && l.StepAttempts+1 < s.cfg.MaxAttempts && dispatched {
		delay := max(time.Second*time.Duration(1<<l.StepAttempts), time.Duration(r.RetryAfterSeconds)*time.Second)
		_, e = tx.Exec(ctx, `UPDATE processor_jobs SET state='deferred',outcome=$3,reason='retry_scheduled',lease_until=NULL,available_at=clock_timestamp()+$4::interval WHERE job_id=$1 AND lease_generation=$2`, l.ID, l.Generation, string(r.Outcome), delay.String())
		if e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	l.Results = append(l.Results, Receipt{r, j.Input.Candidates})
	raw, e := json.Marshal(l.Results)
	if e != nil {
		return e
	}
	if len(raw) > 900000 {
		return errors.New("pipeline_result_limit")
	}
	state, reason := "queued", ""
	if l.Step+1 == len(l.Plan.Steps) {
		src, err := s.fresh(ctx, tx, l, true)
		if err != nil {
			if errors.Is(err, ErrStale) || errors.Is(err, pgx.ErrNoRows) {
				state, reason = "stale", "source_changed"
			} else {
				return err
			}
		} else {
			if err = s.apply(ctx, tx, l, src); err != nil {
				return err
			}
			state = "completed"
		}
	}
	_, e = tx.Exec(ctx, `UPDATE processor_jobs SET results=$3,prepared_job=NULL,step=step+1,step_attempts=0,state=$4,reason=$5,outcome=CASE WHEN $4='queued' THEN '' ELSE $6 END,lease_until=NULL,available_at=clock_timestamp(),finished_at=CASE WHEN $4 IN('completed','stale') THEN clock_timestamp() END WHERE job_id=$1 AND lease_generation=$2`, l.ID, l.Generation, string(raw), state, reason, string(r.Outcome))
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) abandon(ctx context.Context, l Lease, reason string) {
	p, e := s.pool.Get()
	if e != nil {
		return
	}
	_, _ = p.Exec(ctx, `WITH abandoned AS (
 UPDATE processor_jobs SET state='stale',reason=$3,lease_until=NULL,finished_at=clock_timestamp()
 WHERE job_id=$1 AND state='leased' AND lease_generation=$2 RETURNING job_id,lease_generation)
 UPDATE processor_attempts a SET finished_at=clock_timestamp(),outcome='unknown',reason='source_or_input_invalid'
 FROM abandoned j WHERE a.job_id=j.job_id AND a.lease_generation=j.lease_generation AND a.finished_at IS NULL`, l.ID, l.Generation, reason)
}
func (s *Store) RunOne(ctx context.Context) (bool, error) {
	l, e := s.Claim(ctx)
	if e != nil || l == nil {
		return false, e
	}
	bounded, c := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer c()
	j, e := s.Prepare(bounded, *l)
	if e != nil {
		s.abandon(ctx, *l, "source_or_input_invalid")
		return true, e
	}
	r := v1.Result{Binding: j.Binding, Outcome: v1.Failed, Reasons: []string{"retry_exhausted"}, Evidence: []v1.Evidence{}}
	dispatched := l.StepAttempts < s.cfg.MaxAttempts
	if dispatched {
		if e = s.startAttempt(bounded, *l); e != nil {
			return true, e
		}
		st := l.Plan.Steps[l.Step]
		start := time.Now()
		result, err := s.transport.Process(bounded, st.Entry.Endpoint, st.Entry.AuthEnv, j)
		if err == nil {
			cumulative, _ := json.Marshal(append(l.Results, Receipt{Result: result, Candidates: j.Input.Candidates}))
			if len(cumulative) <= 100000 {
				r = result
			} else {
				r.Reasons = []string{"pipeline_result_limit"}
			}
		} else {
			r.Reasons = []string{failureReason(err)}
		}
		gen := strconv.FormatInt(st.Entry.ExecutionGeneration, 10)
		s.metrics.duration.WithLabelValues(st.Entry.ID, gen, string(st.Stage)).Observe(time.Since(start).Seconds())
		s.metrics.note(st.Entry.ID, gen)
		s.metrics.outcomes.WithLabelValues(st.Entry.ID, gen, string(st.Stage), string(r.Outcome)).Inc()
	}
	return true, s.Finish(ctx, *l, j, r, dispatched)
}

// Only bounded implementation-owned reason codes reach durable technical errors.
func failureReason(err error) string {
	switch err.Error() {
	case "binding_mismatch", "invalid_outcome", "result_bounds", "unproven_complete_miss", "unexpected_proposal", "missing_proposal", "one_proposal_per_stage", "invalid_content_type", "invalid_telemetry", "invalid_telemetry_time", "invalid_exclusion", "unexpected_exclusion", "invalid_language", "service_unavailable", "transport_failed", "transport_credential_unavailable", "invalid_uncertainty", "incompatible_contract":
		return err.Error()
	default:
		return "malformed_response"
	}
}
