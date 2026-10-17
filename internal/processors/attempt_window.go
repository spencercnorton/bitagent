package processors

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

const technicalFailureWindowSize = 500

// attemptWindow measures a fixed terminal-attempt population. It does not use
// a jobs-page sample, mix execution generations, or infer success for an
// interrupted request. The retention floor and sample share one SQL snapshot.
func attemptWindow(ctx context.Context, tx pgx.Tx, entry Entry, now string) (map[string]v1.Measurement, error) {
	var count, failures, unknown, deferred, progress, unfinished int64
	var first, last *time.Time
	var executionStart *time.Time
	var floor time.Time
	err := tx.QueryRow(ctx, `WITH recent AS (
 SELECT finished_at,outcome FROM processor_attempts
 WHERE processor_id=$1 AND generation=$2 AND finished_at IS NOT NULL
 ORDER BY finished_at DESC,job_id DESC,lease_generation DESC LIMIT $3
), floor AS (SELECT attempt_history_floor AS value FROM processor_control WHERE singleton)
SELECT count(*),count(*) FILTER(WHERE outcome='failed'),
 count(*) FILTER(WHERE outcome NOT IN('completed','abstained','deferred','failed','excluded','unmatched')),
 count(*) FILTER(WHERE outcome='deferred'),
 count(*) FILTER(WHERE finished_at>(SELECT value FROM floor)),min(finished_at),max(finished_at),
 (SELECT value FROM floor),(SELECT execution_started_at FROM processor_registry WHERE id=$1 AND execution_generation=$2),
 (SELECT count(*) FROM processor_attempts WHERE processor_id=$1 AND generation=$2 AND finished_at IS NULL)
FROM recent`, entry.ID, entry.ExecutionGeneration, technicalFailureWindowSize).
		Scan(&count, &failures, &unknown, &deferred, &progress, &first, &last, &floor, &executionStart, &unfinished)
	if err != nil {
		return nil, err
	}
	start, end := now, now
	if first != nil {
		start, end = stamp(*first), stamp(*last)
	}
	population := "latest 500 terminal attempts for processor execution generation " + strconv.FormatInt(entry.ExecutionGeneration, 10)
	result := map[string]v1.Measurement{}
	put := func(key string, value *float64, reason string) {
		m := measurement(value, "count", start, end, population, "core_postgres")
		m.UnavailableReason = reason
		result["technical_failure_window_"+key] = m
	}
	complete := count == technicalFailureWindowSize && first != nil && first.After(floor)
	if !complete {
		reason := "insufficient_attempt_history"
		if count == technicalFailureWindowSize {
			reason = "retention_history_incomplete"
		}
		put("attempts", nil, reason)
		put("failures", nil, reason)
		put("unknown", nil, reason)
		put("deferred", nil, reason)
	} else {
		put("attempts", number(float64(count)), "")
		put("unknown", number(float64(unknown)), "")
		put("deferred", number(float64(deferred)), "")
		if unknown != 0 {
			put("failures", nil, "unresolved_attempt_outcomes")
		} else if deferred != 0 {
			// A free-form service reason cannot prove whether deferral was a
			// scheduling choice or a technical dependency failure.
			put("failures", nil, "deferred_attempts_unclassified")
		} else {
			put("failures", number(float64(failures)), "")
		}
	}
	result["technical_failure_window_progress"] = measurement(number(float64(progress)), "count", stamp(floor), now,
		"retained terminal attempts after the completeness floor, capped at 500", "core_postgres")
	result["technical_failure_window_unfinished"] = measurement(number(float64(unfinished)), "count", now, now,
		"unfinished retained attempts for the processor execution generation; not counted as successes", "core_postgres")
	// Startup collection is a separately named population, never a fabricated
	// 500-attempt denominator. Both its known execution boundary and retention
	// floor must prove that no earlier current-generation attempts are missing.
	prefixReason := ""
	prefixStart := now
	switch {
	case count >= technicalFailureWindowSize:
		prefixReason = "fixed_window_population_reached"
	case executionStart == nil:
		prefixReason = "execution_start_unknown"
	case !executionStart.After(floor) || first != nil && first.Before(*executionStart):
		prefixReason = "retention_history_incomplete"
	}
	if executionStart != nil {
		prefixStart = stamp(*executionStart)
	}
	for key, n := range map[string]int64{"attempts": count, "failures": failures, "unknown": unknown, "deferred": deferred, "unfinished": unfinished} {
		reason := prefixReason
		if reason == "" && key == "failures" {
			if unknown != 0 {
				reason = "unresolved_attempt_outcomes"
			} else if deferred != 0 {
				reason = "deferred_attempts_unclassified"
			}
		}
		var value *float64
		if reason == "" {
			value = number(float64(n))
		}
		m := measurement(value, "count", prefixStart, now, "complete startup prefix (0-499 terminal attempts) for processor execution generation "+strconv.FormatInt(entry.ExecutionGeneration, 10), "core_postgres")
		m.UnavailableReason = reason
		result["technical_failure_prefix_"+key] = m
	}
	return result, nil
}

// pruneJobs records the newest removed attempt before the FK cascade. Raising
// this floor prevents a surviving older long-running job from filling a hole
// in a supposedly complete window. It never changes the execution epoch.
func pruneJobs(ctx context.Context, tx pgx.Tx, retention time.Duration) error {
	_, err := tx.Exec(ctx, `WITH doomed AS (
 SELECT job_id FROM processor_jobs WHERE finished_at<clock_timestamp()-$1::interval
 ORDER BY finished_at LIMIT 128
), boundary AS (
 SELECT max(COALESCE(a.finished_at,a.started_at)) AS value
 FROM processor_attempts a JOIN doomed d USING(job_id)
), advance AS (
 UPDATE processor_control SET attempt_history_floor=GREATEST(attempt_history_floor,(SELECT value FROM boundary))
 WHERE singleton RETURNING singleton
)
DELETE FROM processor_jobs WHERE job_id IN(SELECT job_id FROM doomed)`, retention.String())
	return err
}
