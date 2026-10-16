package processors

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
)

func measurement(value *float64, unit, start, end, population, source string) v1.Measurement {
	m := v1.Measurement{Value: value, Unit: unit, Window: v1.Window{Start: start, End: end}, Population: population, Source: source}
	if value == nil {
		m.UnavailableReason = "not_measured"
	}
	return m
}
func number(n float64) *float64 { return &n }
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// Refresh records only successful observations as fresh. A failure retains its
// previous observed identity/time and separately marks present health unknown.
func (s *Store) Refresh(ctx context.Context) error {
	es, e := s.Entries(ctx)
	if e != nil {
		return e
	}
	s.active.Store(len(es) > 0)
	for _, entry := range es {
		bounded, c := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		observed, err := s.transport.Status(bounded, entry.Endpoint, entry.AuthEnv)
		c()
		p, e := s.pool.Get()
		if e != nil {
			return e
		}
		code := ""
		if err != nil {
			code = "service_unavailable"
		} else if observed.Processor != entry.Manifest.Processor || observed.Contract != v1.Contract {
			code = "service_identity_mismatch"
		}
		if code != "" {
			_, e = p.Exec(ctx, `UPDATE processor_registry SET last_error=$3 WHERE id=$1 AND generation=$2`, entry.ID, entry.Generation, code)
		} else {
			raw, _ := json.Marshal(observed)
			_, e = p.Exec(ctx, `UPDATE processor_registry SET observed=$3,observed_at=clock_timestamp(),last_error='' WHERE id=$1 AND generation=$2`, entry.ID, entry.Generation, string(raw))
		}
		if e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) Status(ctx context.Context) (v1.Status, error) {
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	now := stamp(time.Now())
	out := v1.Status{Contract: v1.StatusContract, ObservedAt: now, CoreReadiness: "ready", Processors: []v1.ProcessorStatus{}, Queue: map[string]v1.Measurement{}, Limits: map[string]int{"concurrency": s.cfg.Concurrency, "queue_capacity": s.cfg.QueueCapacity, "retained_jobs": s.cfg.RetainedJobs, "max_attempts": s.cfg.MaxAttempts, "request_timeout_ms": int(s.cfg.RequestTimeout.Milliseconds())}}
	tx, e := s.begin(ctx)
	if e != nil {
		return out, e
	}
	defer rollback(tx)
	plan, _, e := readPlan(ctx, tx, s.policy)
	if e != nil {
		return out, e
	}
	out.PipelineRevision = digest(plan)
	es, e := entries(ctx, tx)
	if e != nil {
		return out, e
	}
	for _, entry := range es {
		p := v1.ProcessorStatus{Configuration: entry.Configuration, AuthEnv: entry.AuthEnv, ID: entry.ID, Generation: entry.Generation, ExecutionGeneration: entry.ExecutionGeneration, Installed: true, Configured: true, Enabled: entry.State == "active", State: entry.State, Mode: entry.Mode, OperatingMode: "unavailable", Endpoint: entry.Endpoint, Priority: entry.Priority, Contract: entry.Manifest.Contract, Processor: entry.Manifest.Processor, Capabilities: entry.Manifest.Capabilities, Compatibility: "unknown", Health: "unknown", Readiness: "unknown", Qualification: entry.Manifest.Qualification, Metrics: map[string]v1.Measurement{}}
		var raw []byte
		var observedAt *time.Time
		if e = tx.QueryRow(ctx, `SELECT observed,observed_at,last_error FROM processor_registry WHERE id=$1`, entry.ID).Scan(&raw, &observedAt, &p.LastError); e != nil {
			return out, e
		}
		var observed v1.ServiceStatus
		if len(raw) > 0 && json.Unmarshal(raw, &observed) == nil {
			p.ObservedProcessor = &observed.Processor
			p.InstanceID = observed.InstanceID
			p.StartedAt = observed.StartedAt
			if observedAt != nil {
				p.LastCheckedAt = stamp(*observedAt)
			}
			if observedAt != nil && time.Since(*observedAt) < 2*time.Minute && p.LastError == "" && observed.Processor == entry.Manifest.Processor {
				p.Compatibility = "compatible"
				p.Health = "healthy"
				p.Readiness = observed.State
				p.OperatingMode = entry.Mode
				loaded := observed.State == "ready" || observed.State == "draining"
				p.Loaded = &loaded
			}
		}
		if p.LastError == "service_identity_mismatch" {
			p.Compatibility = "incompatible"
			p.Health = "degraded"
		}
		if p.LastError == "service_unavailable" {
			p.Health = "unavailable"
		}
		pop := "retained attempts for processor execution generation " + strconv.FormatInt(entry.ExecutionGeneration, 10)
		var attempts, unique, retries, waitN, inferenceN int64
		var waitMS, inferenceMS *float64
		var first *time.Time
		e = tx.QueryRow(ctx, `SELECT count(*),count(DISTINCT j.info_hash),count(*)-count(DISTINCT(a.job_id,a.stage)),avg(a.queue_wait_ms),count(a.queue_wait_ms),avg(a.inference_ms),count(a.inference_ms),min(a.started_at) FROM processor_attempts a JOIN processor_jobs j USING(job_id) WHERE a.processor_id=$1 AND a.generation=$2`, entry.ID, entry.ExecutionGeneration).Scan(&attempts, &unique, &retries, &waitMS, &waitN, &inferenceMS, &inferenceN, &first)
		if e != nil {
			return out, e
		}
		start := now
		if first != nil {
			start = stamp(*first)
		}
		for key, value := range map[string]float64{"attempts": float64(attempts), "unique_torrents": float64(unique), "retries": float64(retries), "queue_wait_samples": float64(waitN), "inference_samples": float64(inferenceN)} {
			p.Metrics[key] = measurement(number(value), "count", start, now, pop, "core_postgres")
		}
		p.Metrics["queue_wait_ms"] = measurement(waitMS, "milliseconds", start, now, pop, "core_postgres")
		p.Metrics["inference_ms"] = measurement(inferenceMS, "milliseconds", start, now, pop, "service_reported_retained_attempts")
		for _, outcome := range []string{"completed", "abstained", "deferred", "failed", "excluded", "unmatched"} {
			var n float64
			if e = tx.QueryRow(ctx, `SELECT count(*) FROM processor_attempts WHERE processor_id=$1 AND generation=$2 AND outcome=$3`, entry.ID, entry.ExecutionGeneration, outcome).Scan(&n); e != nil {
				return out, e
			}
			p.Metrics[outcome] = measurement(number(n), "count", start, now, pop, "core_postgres")
		}
		window, err := attemptWindow(ctx, tx, entry, now)
		if err != nil {
			return out, err
		}
		for key, value := range window {
			p.Metrics[key] = value
		}
		telemetryAt := observed.Telemetry.ObservedAt
		var rss, cpu *float64
		if telemetryAt != "" && p.Health == "healthy" {
			if observed.Telemetry.RSSBytes != nil {
				rss = number(float64(*observed.Telemetry.RSSBytes))
			}
			cpu = observed.Telemetry.CPUMilliseconds
		}
		p.Metrics["rss_bytes"] = measurement(rss, "bytes", telemetryAt, telemetryAt, "last measured service computation", "service_reported")
		p.Metrics["cpu_milliseconds"] = measurement(cpu, "milliseconds", telemetryAt, telemetryAt, "last measured service computation", "service_reported")
		out.Processors = append(out.Processors, p)
	}
	pending := 0
	for _, state := range []string{"queued", "leased", "deferred", "completed", "failed", "canceled", "stale"} {
		var n int
		if e = tx.QueryRow(ctx, `SELECT count(*) FROM processor_jobs WHERE state=$1`, state).Scan(&n); e != nil {
			return out, e
		}
		out.Queue[state] = measurement(number(float64(n)), "count", now, now, "current retained jobs", "core_postgres")
		if state == "queued" || state == "leased" || state == "deferred" {
			pending += n
		}
	}
	var oldest *float64
	if e = tx.QueryRow(ctx, `SELECT extract(epoch FROM clock_timestamp()-min(created_at)) FROM processor_jobs WHERE state IN('queued','deferred')`).Scan(&oldest); e != nil {
		return out, e
	}
	out.Queue["oldest_wait_seconds"] = measurement(oldest, "seconds", now, now, "current waiting jobs", "core_postgres")
	out.Queue["capacity_remaining"] = measurement(number(float64(max(0, s.cfg.QueueCapacity-pending))), "count", now, now, "optional admission slots", "core_postgres")
	return out, nil
}
func (s *Store) updateGauges(ctx context.Context) {
	status, e := s.Status(ctx)
	if e != nil {
		return
	}
	s.metrics.registry.Reset()
	active := map[string]bool{}
	for _, p := range status.Processors {
		active[p.ID+"/"+strconv.FormatInt(p.ExecutionGeneration, 10)] = true
		s.metrics.registry.WithLabelValues(p.ID, strconv.FormatInt(p.ExecutionGeneration, 10), p.Processor.Version, p.Processor.ModelVersion, p.Processor.PolicyVersion).Set(1)
	}
	s.metrics.prune(active)
	for k, m := range status.Queue {
		if m.Value != nil && k != "oldest_wait_seconds" && k != "capacity_remaining" {
			s.metrics.jobs.WithLabelValues(k).Set(*m.Value)
		}
	}
}
