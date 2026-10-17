# Optional processor operational controls

These are additive, authenticated core operator extensions. They do not change
the `bitagent.processor.v1` service protocol or allow a service to administer
itself. Check `GET /api/processors/v1/capabilities` before using them. Its
`bitagent.processors.operations.v1` feature map currently advertises
`terminal_attempt_window: latest-500-v1`,
`terminal_attempt_prefix: complete-startup-prefix-v1`,
`admission: bitagent.processors.admission.v1`,
`job_export: bitagent.processors.job-export.v1`,
`reprocessing: bitagent.processors.reprocessing.v1`, and
`preparation_recovery: bounded-local-retries-v1`. Unknown features may be ignored;
unknown required versions must fail closed. Existing core status/registry wire
types remain valid. Absence is not a measured zero or permission to activate.

## Local preparation recovery

Before disclosure, the core rechecks authority and prepares the bounded input.
A changed or missing source is stale. Other preparation errors, including a
database deadline, retain the same job and input for bounded retry. A separate
durable `preparation_failures` counter caps failures across that job at the
configured `max_attempts`; retry delays are 1, 2, 4 and 8 seconds as applicable.
Exhaustion leaves state/outcome `failed` and reason `preparation_failed`, with
no catalogue application or exclusion. Registration changes and cancellation
invalidate the lease; recovery cannot revive it. A restart retains the counter.

Job summaries and exports include `preparation_failures` when nonzero. Status
includes `queue.preparation_failures` across current retained jobs and
`limits.max_preparation_failures`. Negotiate the feature before treating an
absent job counter as zero. These failures never create a service attempt or
enter the latest-500/startup-prefix dispatch population. Assess queue failures
and waiting age alongside service measurements. Raw database errors are not
stored in public reason fields. The bounded job audit retains the counter and
last state/reason, not a timestamped event for every local preparation error.

## Cohort admission

`GET /admission` returns `AdmissionPolicy` from `processorapi/v1/operations.go`:
contract, generation, algorithm, seed, basis_points and policy_version.
`PUT /admission` accepts expected_generation, seed, basis_points, policy_version
and reason. Seed is exactly 64 lowercase hexadecimal characters. Policy version
and reason are 1–96 ASCII letters/digits/dots/underscores/hyphens, starting with a
letter or digit; use a bounded reason code, never credentials or user data.

`basis_points` is 0–10000: 100 means 1%, 1000 means 10%, and 10000 means all
otherwise eligible inputs. Zero stops optional admission; the core crawler,
metadata persistence, deterministic processing and eligible serving continue.
The initial policy includes all inputs for compatibility with the previous
release. Set an explicit limited policy **before** enabling an unqualified or
limited-rollout service. A service registration cannot reset this policy.
This setting is pipeline-wide so sampling cannot bypass a participant or alter
stage ordering, result precedence or policy exclusions.

The published `sha256-seed-infohash-v1` selection rule is:

1. Concatenate UTF-8 `bitagent.processors.admission.v1\n` (including a single
   newline), the decoded 32-byte seed, and the 20-byte torrent info hash.
2. Calculate SHA-256. Interpret its first eight bytes as an unsigned big-endian
   integer and take its remainder modulo 10000.
3. Admit when that bucket is strictly less than basis_points, subject to every
   existing privacy, name, catalogue, capacity and lifecycle check.

This is a reproducible hash cohort, not a promise of an exact observed sample
count. The same seed produces nested 1%, 10% and full cohorts; metadata edits do
not move a torrent between buckets. Published independent vectors are in
`fixtures/admission-cohorts.json`. Operators must freeze the selected seed,
policy version and population before interpreting rollout evidence. This rule
is newly defined here; it does not retroactively supply a missing research
sampling protocol or establish accuracy, reference quality or promotion.

Writes use compare-and-swap; stale generations return 409. An identical write
with the current generation is a no-op. After a lost response, read current
policy and reconcile exact values before any retry. Changes append an audit
entry, invalidate in-flight jobs, reset the scan cursor, and advance **all**
registrations' administrative and execution generations. Clients must refetch
registry generations; a stale registry view cannot activate or replace a
service. Failure windows start fresh for the new populations. Existing facts,
exclusions and receipts remain intact. Dispatched interrupted requests become
unknown, and late results cannot apply. The full admission policy is bound into
the persisted plan and pipeline revision of every new job.

`GET /admission/history?before=0&limit=50` returns `AdmissionHistory`: items have
policy, reason and recorded_at. A nonzero next_before is the exclusive generation
cursor for the next page. The default page contains 50 and the maximum 100.
This audit describes changes made using the core's shared administration
credential; it does not identify an individual person behind that credential.

Schema 63 initializes this policy and invalidates pre-policy plans without
rewriting completed evidence. A destructive downgrade is refused once operator
changes exist. A binary-only downgrade could ignore the gate and is not a safe
rollback: stop optional work and restore the matching reviewed configuration,
image and database using the documented [recovery procedure](migration.md).

## Failure-window use

The status [measurement contract](operator-api.md) defines the exact terminal
population, conservative completeness floor, null reasons, deferred and unknown
counts. A complete window does not prove absence of adverse scientific or
privacy events. Consumers must also evaluate unfinished work and queue health.
Core exposes evidence and controls; it does not choose a model promotion policy
or fabricate an approval when evidence is missing.
Fresh executions also expose a separately named, proved complete startup prefix
so an explicitly authorized initial collection phase can gather its first 500
observations. This does not change the fixed-window denominator or turn missing
history into success. Schema 65 leaves historical execution start times unknown;
registration, pause/mode activation and admission changes establish new starts.

## Retained job evidence export

`GET /jobs/:job_id/export` captures a read-only, repeatable database snapshot in
`bitagent.processors.job-export.v1`. It includes the job summary, frozen input,
source snapshot, ordered plan, retained prepared step (nullable), accepted
terminal step receipts, attempt timing/outcome history, and current latest
application/restriction records for the torrent. Those latest records may name
a different job; they are not rewritten as historical facts. Removed plugins
remain identified by the immutable versions in their job. Export never executes
a service, retries a job or changes catalogue state.

The response is bounded to 4 MiB and 1000 attempts. An exceeded bound returns
413, not a silently truncated archive. A pruned/missing job returns 404; this is
not a statement that it never ran. Archive the full response bytes and their
digest before retention expires. Go clients can use `v1.DecodeExport`, which
retains strict duplicate/unknown-field checks at this larger operator limit;
service messages retain their 256 KiB limit. These exports contain torrent
metadata and require the administration credential. They are not public status
responses or an alternate channel for giving plugins database access.

`coverage` explicitly separates retained core evidence from unavailable payloads.
The core does **not** retain exact HTTP request/response bytes for every attempt,
does not receive a plugin's provider/model payloads or independent references,
and may hold only a bounded file inventory. Retry attempt history contains
outcomes/reasons, not every response body. A prepared step has its frozen
semantic input but is not proof of the deadline bytes used in every dispatch.
Full raw service/provider/reference archives must be captured and validated at
their owning boundary. An export or hash alone cannot fill these gaps or prove
research provenance, model accuracy or production promotion readiness.

## Audited reprocessing

`POST /jobs/:job_id/reprocess` accepts `ReprocessWrite`: a new 64-character
lowercase hexadecimal request_id, the origin job's expected_input_revision,
the **current** status expected_pipeline_revision, and a bounded reason code.
The origin must be a retained terminal job. This is a reviewable operator
request, not a service ability to retry or modify its own catalogue decisions.
The response and `GET /reprocessing/:request_id` use `ReprocessStatus` with
contract `bitagent.processors.reprocessing.v1`.

Core atomically retains the origin export, current source/application/restriction
before-images and reason, then enqueues one ordinary `process_torrent` job in
the existing PostgreSQL queue. Default matching preserves known type/match and
current manual authority; rematch, alternate workflows and filter bypass flags
are forbidden on these messages. A destructive classifier/filter verdict retains
the torrent and becomes a deferred processing receipt. Independent policy and
privacy checks still apply; reprocessing does not remove a restriction or
authorize publication. Correcting an established wrong identity or rescinding
an exclusion requires its separate core/operator authority review, not this API.

The operation starts as `waiting_core`. Queue success alone is insufficient:
core requires a processing receipt correlated with this request, rechecks its
source digest/current authority and the exact expected pipeline, then marks it
`ready`. Bounded priority admission creates a new optional job and returns
`queued` with result_job_id. The request ID participates in the source revision,
so semantic retry IDs remain stable within that new job while differing from
the original. Normal processing outcomes remain truthful: completed processing
does not fabricate a completed unsuccessful match when no search ran. Plugins
still receive the actual matching outcome and must respect their own evidence
requirements. After optional settlement, inspect the job's results/outcome;
`settled` alone is not a claim of successful matching, changed catalogue facts,
completed scientific qualification or indexer delivery.

Other request states are explicit: `core_only` means normal processing completed
with no active optional plan; `held` denotes unavailable correlated evidence or
current authority/cohort denial; `deferred` denotes incomplete or expired normal
work/admission; `failed` denotes a failed core job; `stale` denotes a changed
source or pipeline; `canceled` records operator cancellation. Requests cannot
bypass a rollout cohort. Changing registration/admission may stale the request;
review current state before issuing a new request ID. The ordinary core queue
keeps its existing bounded retry behavior. A request waiting for core/admission
expires after one hour rather than remaining silently pending forever.

Duplicate delivery with the same origin and complete request body returns the
existing status, including after the origin job expires. Reusing the ID for a
different body, stale revisions, or concurrent work on the same torrent returns
409. Capacity returns 429. Outstanding requests are capped at
`min(queue_capacity,64)` and retained audit requests at
`min(retained_jobs,1000)`, exposed in status limits. Audit history is not silently
pruned to make room. Archive and reconcile it through reviewed maintenance when
capacity is exhausted; a refusal does not affect crawling or ordinary queues.

`POST /reprocessing/:request_id/cancel` is idempotent. It stops that request's
optional admission and invalidates any corresponding optional lease. Already
settled facts remain unchanged. An already queued ordinary processing pass may
finish; its destructive behavior remains disabled and its late receipt cannot
reactivate the canceled request. Independently triggered ordinary processing and
source revisions retain their usual behavior.

`GET /reprocessing/:request_id/export` returns `ReprocessingAudit` under the same
contract: current request status, structured origin export, immutable before
snapshot, and the correlated core receipt when available. It is bounded to
8 MiB, requires administration auth, and sends `Cache-Control: no-store`.
The before-images survive transient job retention. JSONB preserves structured
values, not the original HTTP serialization; all raw-payload gaps described
above remain explicit. Schema 64 refuses a destructive downgrade when these
audit requests exist. Restore the matched database/image/configuration for a
rollback, retaining intervening evidence and restrictions for reconciliation.
