# Migration and rollback

Version 5 removes embedded inference and its worker/CLI/configuration surface.
This is a major-version boundary. The default installation needs only the core
and its existing PostgreSQL database. No broker, model runtime, vector database,
provider account or plugin is required. UI and individual plugin packaging are
separate releases.

## Before upgrading

1. Record the running image digest, database migration version, core worker
   configuration, catalogue counts, queue age/depth and persisted-metadata rate.
   Keep a tested, consistent PostgreSQL backup and all recovery/capture exports.
2. Stop old writers before migration. In particular stop `llm_work`, synchronous
   junk inference and Batch submission/settlement processes. Do not run old and
   new processing binaries concurrently against this database.
3. Inventory pending hosted Batch IDs and dispatch fences. A provider may still
   have an accepted job after local shutdown. Preserve those IDs and results for
   explicit provider-side reconciliation; the new core neither resubmits nor
   cancels them. Retained `intent`/`unknown` fences are not permission to retry.
4. Remove model runtimes, provider URLs, API keys, prompts, inference budgets and
   old `CLASSIFIER_LLM_*`, `CLASSIFIER_LLM_MATCH_*`, `CONTENT_FILTER_LLM_*`,
   `LLM_WORK_*`, `LLM_EVALUATION_CAPTURE_*` and junk-inference configuration from
   the core deployment and secret mounts. Keep historical evidence in restricted
   storage. `eval-freeze`, `eval-replay`, `matcher-eval`, `batch-llm-match` and the
   model-readiness tool are retired from the executable. Their source is frozen
   in `_legacy`, and the pre-split commit reproduces historical evaluations.

## Apply and qualify

1. Upgrade through the reviewed source/release and normal Git-backed deployment
   path. Run migration 61 with old workers stopped. Existing migrations and the
   catalogue recovery cascade schema are unchanged. New tables have no foreign
   key into the recoverable torrent cascade.
2. Migration 61 copies every queued/deferred/leased legacy job's complete
   before-image into `processor_legacy_freeze`, records a held event, then sets
   it to `held` with `embedded_runtime_retired` and clears its lease. Completed
   tasks, applications, dispatch fences, provider results, captures, budgets,
   quarantines and recovery snapshots remain intact. No legacy result is
   reinterpreted as a new plugin result or independently verified reference.
3. Run core workers with no registrations. `worker run --all` includes the new
   `processors` worker. Explicit worker lists must add `processors` and replace
   retired junk inference with `quarantine` only if historical maintenance is
   desired. `JUNKPURGE_MAINTENANCE_ENABLED` remains false by default.
4. Verify startup, metadata persistence, ordinary queue progress, deterministic
   processing, GraphQL/search/Torznab, recovery and existing serving exclusions.
   On an actual deployment, verify the running image digest and container
   `StartedAt` after the merge, then compare equivalent crawl windows after DHT
   warmup. The synthetic validation report does not replace this production
   qualification.
5. Historical attached identities, type facts and English-audio provenance stay
   stored. Ordinary reprocessing retains known types/matches; explicit rematch
   remains the way to recompute them. New deterministic audio evidence may
   supersede older model claims under the existing precedence. No plugin can
   clear a quarantine or treat a historic model claim as physical verification.

## Optional registration

Set the generic admin credential once and restrict the operator API to trusted
callers. Deploy a separately supervised service with CPU, RAM, PID, storage,
request and network limits; it receives no database credential. Register its
endpoint in shadow mode with generation 0, a reviewed `expected_processor`
identity and configuration satisfying its manifest. Registration checks the live
manifest and status before committing. An unavailable service cannot prevent
core startup.

The queue catches up existing eligible source rows through a bounded keyset
scan. On a large catalogue, catch-up is intentionally gradual. It does not
manufacture past normal-processing receipts: a plugin requiring a current
receipt must wait for ordinary reprocessing. `processing_outcome=completed`
means the real normal pipeline finished on this raw metadata and core policy;
`matching_outcome=not_attempted` still means no applicable matcher ran. A clean
match miss additionally requires the matching catalogue-row receipt.

Plugin authentication uses a dedicated `BITAGENT_PROCESSOR_TOKEN_*` reference.
The transport reads that environment value, or a file with exactly that name
under `PROCESSORS_CREDENTIAL_DIRECTORY` (default
`/run/secrets/bitagent-processors`). Mount the directory read-only. Atomic file
replacement adds/rotates credentials without a core restart. These are service
transport credentials; model/provider credentials stay inside the service.
Configuration rejects credential-like keys and is returned to authorized
operators for exact acknowledgement reconciliation.

## Upgrade, drain, rollback and retirement

Warm and test a replacement at a second endpoint. PUT the full desired settings,
current generation and exact expected identity. A competing update produces
409. An identity mismatch produces 422. Lost acknowledgements must be reconciled
against all read-back fields; do not assume a matching version alone proves
which update won.

PATCH `state=draining` stops admission of new optional pipelines while existing
jobs settle; the execution generation stays constant. Because one job freezes
an ordered multi-plugin plan, draining any participant drains pipeline admission
as a whole. `paused`, mode changes, replacement and removal invalidate pending
work. Cancellation is logical and immediate in storage; remote execution is
bounded by its deadline and any late delivery is rejected.

Rollback registers the old endpoint and reviewed full identity under a fresh
generation. It changes future execution. It does not destructively reverse
already applied catalogue facts, discard evidence or clear policy exclusions.
Remove a registration to return to zero-plugin operation. Existing eligible
facts remain available. A failed/deferred apply step prevents new enrichment
from that pipeline; explicit validated restrictions can still be retained.

Publication exclusions are restriction-only records with their exact processor
binding. They survive plugin removal and rollback. Release one only through an
explicit core/operator policy review, preserving its binding and reason in the
operator's audit before a controlled database change. Neither an abstention nor
service disappearance performs that review.

Do not downgrade the schema on a live catalogue to force an older binary to
start. The down migration refuses to discard registrations, jobs, applications,
exclusions, receipts or frozen legacy work. Core-image rollback requires a
matched backup restored to an isolated database, the prior reviewed image, and
reconciliation of writes and restrictions since that backup before cutover.
An empty-schema down/up drill is supported; evidence-bearing downgrade is
intentionally rejected. Keep old inference disabled even during rollback unless
its outstanding provider work and exact previous policy have been reconciled.

## Retention and recovery

Pending work is capped independently from the core queue. Optional requests have
separate global lease slots, deadlines and per-step attempt caps. Terminal jobs
and their attempt histories have bounded retention and a total cap; when full,
new optional admission waits. The most recent applied receipt per torrent and
policy exclusions are retained separately so job cleanup cannot immediately
replay an application or remove a restriction. Keep exported job histories when
longer-term evaluation provenance is required. Historical pre-v5 evidence keeps
its existing retention policy.

Schema 62 adds a conservative terminal-attempt completeness floor and an index
for the latest-500 technical failure measurements. Existing attempts remain
unchanged. After upgrade, these measurements are unavailable until 500 fresh
terminal attempts for the current execution generation are retained. Pruning
advances the floor before deleting attempt history, so a surviving older row
cannot fill a missing part of the population. Operational gates must treat null
as unavailable, never as zero failures. This additive migration can be reversed
for an isolated schema-61 rollback; it does not relax schema 61's refusal to
discard processor or legacy evidence when downgrading farther.

After service/worker crashes, expired leases are reclaimed with a larger lease
generation. Requests reuse frozen semantic inputs and a stable step ID; only the
attempt deadline changes. Successful completion and application commit together.
Exhausted deferred/technical work is retained as that outcome, not converted to
an unsuccessful match or junk. To intentionally reconsider it after repair,
register a new execution generation; ordinary source changes also create new
work. Core metadata and deterministic processing never wait for this queue.
