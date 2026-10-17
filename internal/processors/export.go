package processors

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

var jobIdentity = regexp.MustCompile(`^[0-9a-f]{64}$`)
var errExportLimit = errors.New("export_limit")

func (s *Store) ExportJob(ctx context.Context, id string) (v1.JobExport, error) {
	out := v1.JobExport{Contract: v1.JobExportContract, SourceSchema: "bitagent.core-source.v1", PlanSchema: "bitagent.processor-plan.v1"}
	if !jobIdentity.MatchString(id) {
		return out, ErrControl
	}
	p, err := s.pool.Get()
	if err != nil {
		return out, err
	}
	// One repeatable snapshot binds the job, attempt history and latest authority
	// records. No source mutation or service/network call occurs during export.
	tx, err := p.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer rollback(tx)
	var input, results, prepared []byte
	var hash []byte
	var step int
	var created, available, captured time.Time
	var lease *time.Time
	err = tx.QueryRow(ctx, `SELECT job_id,info_hash,input_revision,pipeline_revision,state,step,attempts,preparation_failures,
 created_at,available_at,lease_until,outcome,reason,input,source,plan,prepared_job,results,transaction_timestamp()
 FROM processor_jobs WHERE job_id=$1`, id).Scan(&out.Job.JobID, &hash, &out.Job.InputRevision, &out.Job.PipelineRevision,
		&out.Job.State, &step, &out.Job.Attempts, &out.Job.PreparationFailures, &created, &available, &lease, &out.Job.Outcome, &out.Job.Reason,
		&input, &out.SourceSnapshot, &out.FrozenPlan, &prepared, &results, &captured)
	if err != nil {
		return out, err
	}
	out.CapturedAt = stamp(captured)
	out.Job.CreatedAt, out.Job.AvailableAt = stamp(created), stamp(available)
	if lease != nil {
		x := stamp(*lease)
		out.Job.LeaseUntil = &x
	}
	var plan Plan
	for _, pair := range []struct {
		raw  []byte
		into any
	}{{input, &out.Input}, {results, &out.Results}, {out.FrozenPlan, &plan}} {
		if err = json.Unmarshal(pair.raw, pair.into); err != nil {
			return out, err
		}
	}
	if len(prepared) > 0 {
		if err = json.Unmarshal(prepared, &out.PreparedJob); err != nil {
			return out, err
		}
	}
	if len(plan.Steps) > 0 {
		st := plan.Steps[min(step, len(plan.Steps)-1)]
		out.Job.Stage, out.Job.ProcessorID = st.Stage, st.Entry.ID
		out.Job.Processor, out.Job.ExecutionGeneration = st.Entry.Manifest.Processor, st.Entry.ExecutionGeneration
	}
	rows, err := tx.Query(ctx, `SELECT lease_generation,processor_id,generation,stage,started_at,finished_at,
 queue_wait_ms,inference_ms,outcome,reason FROM processor_attempts WHERE job_id=$1 ORDER BY lease_generation LIMIT 1001`, id)
	if err != nil {
		return out, err
	}
	out.Attempts = []v1.AttemptRecord{}
	for rows.Next() {
		var a v1.AttemptRecord
		var start time.Time
		var end *time.Time
		if err = rows.Scan(&a.LeaseGeneration, &a.ProcessorID, &a.ExecutionGeneration, &a.Stage,
			&start, &end, &a.QueueWaitMS, &a.InferenceMS, &a.Outcome, &a.Reason); err != nil {
			rows.Close()
			return out, err
		}
		if len(out.Attempts) >= 1000 {
			rows.Close()
			return out, errExportLimit
		}
		a.StartedAt = stamp(start)
		if end != nil {
			v := stamp(*end)
			a.FinishedAt = &v
		}
		out.Attempts = append(out.Attempts, a)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT
 (SELECT to_jsonb(a)-'info_hash' FROM processor_applications a WHERE info_hash=$1),
 (SELECT to_jsonb(e)-'info_hash' FROM processor_exclusions e WHERE info_hash=$1)`, hash).
		Scan(&out.LatestApplication, &out.LatestRestriction)
	if err != nil {
		return out, err
	}
	out.Coverage = map[string]string{
		"frozen_input": "retained_core_input", "source_snapshot": "bounded_core_source",
		"accepted_results": "retained_terminal_step_receipts", "attempt_results": "outcomes_and_reasons_only",
		"raw_service_request_response": "not_retained_by_core", "raw_provider_payloads": "not_received_by_core",
		"independent_references": "not_received_by_core", "latest_authority": "current_at_capture_may_name_another_job",
		"file_inventory": "partial",
	}
	if out.Input.FilesComplete {
		out.Coverage["file_inventory"] = "complete_for_frozen_core_input"
	}
	b, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if len(b) > v1.MaxExportBody {
		return out, errExportLimit
	}
	return out, nil
}
