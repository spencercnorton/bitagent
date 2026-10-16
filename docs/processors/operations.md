# Optional processor operational controls

These are additive, authenticated core operator extensions. They do not change
the `bitagent.processor.v1` service protocol or allow a service to administer
itself. Check `GET /api/processors/v1/capabilities` before using them. Its
`bitagent.processors.operations.v1` feature map currently advertises
`terminal_attempt_window: latest-500-v1` and
`admission: bitagent.processors.admission.v1`. Unknown features may be ignored;
unknown required versions must fail closed. Existing core status/registry wire
types remain valid. Absence is not a measured zero or permission to activate.

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
