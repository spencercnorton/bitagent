# Recoverable catalogue removal foundation

This opt-in library is **disabled and unwired**. The normal application factory
and HTTP registry do not attach it. It changes no classifier, content, liveness,
provider, allowance or serving policy. Policy and independently assessed harm,
retention and storage budgets must be settled before a runtime integration.
Historical unsnapshotted losses remain unrecoverable.

An enabled `cataloguerecovery.Store` requires an explicit recovery window (at
most 365 days), retained canonical JSON bytes, bytes per complete snapshot (at
most 64 MiB), snapshot count, row count per snapshot (at most 65,536), and relation
storage budget. There are no enabled defaults. A complete snapshot over any cap
fails the removal; it is never truncated. Expired and restored snapshots stay
retained and continue consuming capacity. There is no purge or budget refund.

`MaxPayloadBytes` is not a physical disk budget. `MaxStorageBytes` separately
checks the actual recovery tables and the shared verdict tables, including their
indexes and TOAST, before and after a transaction. Failed transactions may still
allocate pages; reserve headroom for the bounded attempted write. PostgreSQL
WAL, unrelated relations, replicas and backups need separate capacity budgets
and monitoring. This guard cannot guarantee a filesystem ceiling. A relation
already over its budget refuses further transitions until an operator remedies
capacity; rollback never silently relaxes a cap.

## Caller contract

Construct the store only with explicit reviewed limits. `RemoveBatch` accepts
1–32 distinct hashes and a bounded reason. It owns a repeatable-read transaction:
locks budget, then ordered hash/raw/dependent rows; captures every column and
its schema; removes raw rows; appends a bound blocking verdict and recovery
transition events; and commits all of these together. Missing tables, unknown raw
foreign-key dependencies, capture/ledger errors and cancellation roll back the
entire batch. Retry SQLSTATE `40001` as a whole operation with current evidence.
A retry of the same committed removal returns its original receipt.

Snapshots include raw rows, shared source definitions and every source
association field, complete files, piece bytes, hints, tags, classified rows,
tracker history and expired purge claims. Single-file NULL counts remain NULL.
Current metadata, evidence, canonical labels, liveness, quarantine snapshots,
paid dispatch fences and application provenance remain in their own tables.
Private torrents, canonical/reference overrides, ID-bearing hints, protected
manual/reference/wanted tags, active purge claims/quarantine and independent
blocking verdicts refuse removal. An integration must also check any external
wantlist at its own admission boundary; this library cannot infer external wants.

`blocking.WithRecovery` explicitly attaches the store to a previously unused
native manager backed by the **same pool**. No production factory calls it.
All deletion/block writers must use this qualified implementation; mixed legacy
and recovery writers are unsupported. The attached `Block` uses the atomic
removal first. A subsequent bloom flush only adds the block and never deletes
raw rows again. A failed bloom flush leaves the durable exact block in force,
without losing its complete recovery receipt. Filter errors fail closed.

`Restore` retains the immutable snapshot and restores complete rows in one
transaction. It checks payload digest, exact column/constraint contract, source
registry equality, dependencies, absence of recrawled raw state, active quarantine
or a new canonical override, and the current **exact** removal verdict receipt.
Changed source, schema, competing verdict or expiry refuses restoration. Success
appends its matching restored verdict and unblock receipt in that transaction.
The attached manager can then override its bloom hit only for that current bound
restoration. A new block revokes this exception; repeating an older restore does
not release it. Existing community blocklists and independent serving/crawler
policies continue to apply. No blanket bloom reset is provided.

`recoveryhttp.New(store, operatorAuthentication).Apply(engine)` supplies an
explicit operator-only `POST /catalogue-recovery/:id/restore` route. It requires
authentication middleware, but is absent from the normal HTTP registry. It
returns whether this call restored rows; already-restored calls are idempotent.
It does not expose snapshot contents. Mount it only on an authenticated operator
surface when the same recovery integration has been qualified.

## Verification

The disposable PostgreSQL regressions use the full migration history and actual
raw/file/source tables. They cover exact multi-file and intentional single-file
NULL round trips; current source, schema and verdict conflicts; source/dependency
changes; active quarantine and protected state; payload/row/count/storage caps;
concurrent capacity and restore; event-write rollback; server-backend termination
mid-removal and successful retry; pending bloom flush after restore; and reload of
the persisted bloom. A real authenticated restore request verifies the recovered
rows, native crawler filter and ordinary Torznab HTTP serving. All fixtures are
synthetic. These establish engineering recoverability, not independent policy
correctness or permission to remove additional catalogue entries.
