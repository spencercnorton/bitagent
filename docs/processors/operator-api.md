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
- `GET /capabilities` returns `bitagent.processors.operations.v1` and a feature
  map. Negotiate extensions using their exact advertised contract. A 404 from
  an older core means unsupported; never emulate an unavailable control by
  editing registry configuration or sampling a jobs page.
- `GET /admission` and `PUT /admission` expose the versioned whole-pipeline
  cohort policy. `GET /admission/history?before=N&limit=50` returns its append-only
  audit, newest first, at most 100 entries. `next_before=0` ends pagination.
  See [operational controls](operations.md) for the write and sampling contract.
- `GET /jobs/:job_id/export` captures retained core evidence with explicit
  coverage gaps. `POST /jobs/:job_id/reprocess` enqueues an audited ordinary core
  pass before optional work. Read, export and cancel these requests through
  `/reprocessing/:request_id`, `/export` and `/cancel`; the
  [operational controls contract](operations.md) defines their bounds/states.

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

The `technical_failure_window_*` measurements use exactly the latest 500
terminal attempts for the current **execution generation**, ordered by
`finished_at DESC, job_id DESC, lease_generation DESC`. They never derive a
denominator from the jobs page or mix replacement processor versions:

- `attempts` is 500 only when a complete retained population is proved.
- `failures` counts technical `failed` outcomes only when the entire population
  can be classified. Semantic abstention, policy exclusion and completed
  unsuccessful matches remain distinct.
- `deferred` counts work deferred by the service. Its free-form reason cannot
  prove whether it represents scheduling or a technical dependency failure.
  When nonzero, `failures` is null with `deferred_attempts_unclassified`.
  The stored deferred outcome is never rewritten as success or failure.
- `unknown` counts interrupted attempts with no settled result. When nonzero,
  `failures` is null with `unresolved_attempt_outcomes`, never a success estimate.
- `progress` counts retained terminal attempts after the completeness floor,
  capped at 500. It is collection progress, not a smaller accepted denominator.
- `unfinished` counts retained attempts that have not reached a terminal state;
  they are outside this terminal population and are not inferred successes.

These keys have the full `technical_failure_window_` prefix. Missing population
produces null with `insufficient_attempt_history`; a retention gap produces
`retention_history_incomplete`. Migration establishes a conservative floor at
upgrade time because older retention cannot be reconstructed. Job pruning raises
that floor to the newest removed attempt; the window becomes usable again only
after 500 newer terminal attempts. This can conservatively withhold measurements
for other processors. No history is fabricated to fill a gap. Expired dispatched
leases settle as technical failure; cancellation, changed registration and stale
input settle as unknown interruption. A request that was never dispatched does
not acquire an attempt. These are operational observations, not accuracy scores
or automatic promotion decisions; consumers must also assess unfinished work,
queue age, independent adverse events and their own approved rollout criteria.

When negotiated as `terminal_attempt_prefix: complete-startup-prefix-v1`, the
separate `technical_failure_prefix_attempts`, `failures`, `unknown`, `deferred`
and `unfinished` keys describe a **complete 0–499 terminal-attempt startup
population**. They require a recorded execution-generation start strictly newer
than the retention floor, and no retained terminal attempt before that start.
Unknown creation time yields null/`execution_start_unknown`; any possible
retention gap yields null/`retention_history_incomplete`. At 500 terminal attempts
these prefix fields become null/`fixed_window_population_reached`; use the full
window instead. Deferred/unknown outcomes also make the prefix failure count
unavailable exactly as above. An explicit fresh prefix can be 0/0/0, with
unfinished work counted separately; this is not a 500-attempt rate or pass.
Operators may use it only under an explicit bounded initial-collection policy,
while respecting all independent adverse-event and scientific gates. The core
never supplies that authorization. Migration leaves old execution start times
unknown; only a new execution generation establishes a provable boundary.

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
The native process collector is available on Linux; clients must treat absent
boot samples as unknown on unsupported platforms. Core request duration is distinct from service-reported inference time. Queue
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
