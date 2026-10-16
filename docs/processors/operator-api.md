# Processor operator API v1

Wire types: `processorapi/v1/operator.go`. Base path `/api/processors/v1`.
All endpoints require `Authorization: Bearer <PROCESSORS_ADMIN_TOKEN>`.
No token configured means operator endpoints return 503; it does not disable
core operation. Plugin transport credentials are separate environment entries
named `BITAGENT_PROCESSOR_TOKEN_*`, referenced by name and never returned.
Model credentials never enter this API or the core configuration.

- `GET /status` returns `Status` with contract `bitagent.processors.status.v1`.
- `GET /registry` returns the same processor array as status.
- `GET /registry/:id` returns one `ProcessorStatus` or 404.
- `PUT /registry/:id` accepts `RegistryWrite`. Creation requires generation 0;
  update requires the current generation. A missing/stale generation returns
  409. Success returns the new `ProcessorStatus`, with generation incremented.
  Core fetches and validates manifest/configuration/readiness before committing
  active endpoint replacement. The replacement must expose the same ID. Optional expected_processor pins the
  full intended identity; a mismatch rejects activation.
  `state` active enables admission, paused stops it and cancels existing work,
  draining stops admission and allows existing compatible work to settle.
  `mode` shadow records proposed results without catalogue writes; apply permits
  only core-supported, validated applications. Default to shadow in clients.
- `PATCH /registry/:id` accepts `RegistryControl`: expected_generation plus
  only the supplied state/mode fields. All deployment settings are preserved.
- `DELETE /registry/:id?expected_generation=N` removes registration and cancels
  obsolete work. Receipts and already-applied facts are retained.
- `GET /jobs?limit=50&cursor=<opaque>` returns `{items: [...], next_cursor: ...}`.
  Limit is at most 100. Optional processor_id filters pipelines containing that
  participant before pagination. Summaries identify the current step (last step
  when terminal), including processor identity and execution_generation. Attempts
  are pipeline totals, not counts for an individual stage. Job summaries contain job_id, input_revision,
  pipeline_revision, state, stage, processor_id, attempts, created_at,
  available_at, lease_until, outcome and reason. Null timestamps are unknown.
- `POST /jobs/:job_id/cancel` is idempotent; returns `{job_id, state:"canceled"}`
  for active jobs. Completed jobs return their actual terminal state.

Registry operations are synchronous and bounded, so there is no asynchronous
operation queue to poll. 400 denotes invalid input, 401 unauthorized, 404 absent,
409 revision conflict, 422 incompatible/unready service, 503 unavailable storage
or disabled administration. Responses never include transport credentials.

## Status and measurement semantics

Installed means a persisted registration exists. Configured means its manifest
and configuration were validated. Enabled reflects active admission. Loaded is
nullable and requires a matching live version observation. Desired `mode` and
`state` are distinct from observed `operating_mode`, `health` and `readiness`.
Configuration and auth_env are returned as persisted settings for exact
acknowledgement reconciliation. auth_env is only a dedicated credential reference.
Core readiness is database/core availability; optional failures never make it
unready. A removed plugin does not remain installed just because history exists.

Metrics have value (nullable), unit, UTC window, population, source and
unavailable_reason. Zero is only a measurement. Standard per-processor keys:
`attempts`, `unique_torrents`, `completed`, `abstained`, `deferred`, `failed`,
`excluded`, `unmatched`, `retries`, `queue_wait_ms`, `queue_wait_samples`,
`inference_ms`, `inference_samples`, `rss_bytes`,
`cpu_milliseconds`. Core counters are over retained durable execution history,
not a hidden lifetime accuracy population. Service resource measurements carry
the service observation window. Unsupported measurements are null.

Global queue keys are queued, leased, deferred, failed, canceled, stale,
completed, oldest_wait_seconds and capacity_remaining. Completed job state means
pipeline execution settled; inspect outcome to distinguish completed proposals,
abstention, exhausted deferral and technical failure. No reason totals are
inferred from a bounded jobs page.

Prometheus series are process-lifetime counters/histograms:

- `bitagent_processors_attempts_total{processor_id,generation}`
- `bitagent_processors_outcomes_total{processor_id,generation,stage,outcome}`
- `bitagent_processors_queue_wait_seconds{processor_id,generation,stage}`
- `bitagent_processors_request_duration_seconds{processor_id,generation,stage}`
- `bitagent_processors_jobs{state}` and `bitagent_processors_admission_total{outcome}`
- `bitagent_processors_registry_info{processor_id,generation,version,model_version,policy_version}`

Here generation is execution_generation, not the administrative CAS generation.
Drain/resume increments CAS while preserving execution identity and counters;
pause, mode changes and replacement invalidate execution. Old label sets are
retired from the current exporter; external time-series storage retains its
history. `bitmagnet_process_start_time_seconds` identifies core restart/reset.
Core request duration is distinct from service-reported inference time. Queue
means include all retained dispatched attempts; inference means include only
attempts with a reported measurement. Sample counts make those populations
explicit. Current status counts may shrink when terminal history expires.

Observed identity, instance_id, started_at and last_checked_at come from the last
successful service observation. A failed poll does not refresh them or assert
loaded=true. More than two minutes without successful observation yields unknown
current health. CPU/RSS use telemetry.observed_at, not poll time. Unknown values
remain null. Service qualification is informational.

`evaluation` is null in v1. A plugin's `qualification` claim and result
`uncertainty` are not measured accuracy, reference quality, publication evidence
or proof of useful indexer delivery. A future evaluation extension needs its
own version, cohort/reference provenance, numerator/denominator, unknown
handling, window and uncertainty. UI should display **not measured** now.
