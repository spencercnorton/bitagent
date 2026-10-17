# Operational controls validation

Historical measurement revision: `f4a41a61e56c342cd01b85c95b1a5bcd52a27551`.
The [machine-readable measurements](operations-validation.json) retain all
samples and their scope. Earlier [5.0 validation](validation.md) remains a
separate historical record.

The identical crawler persistence harness ran against released 5.0.0
(`e59cc02d5dc907c4c6a13671ddb9f98145e1bb00`) and the candidate in ABBA order,
serially, with GOMAXPROCS=2 and a disposable PostgreSQL16 database. Each run
measured five batches of 5000 synthetic torrents and asserted exact torrent,
piece, source-link and queue populations. All assertions passed. Median rates
were 9676.8 versus 9312.7 torrents/second: **3.76% lower** for the candidate in
this local run. Baseline samples ranged 8220.4–10602.9; candidate samples ranged
7793.2–10298.3. Overlapping variability is not a throughput improvement or a
production non-inferiority result. The final request-expiry guard was added
after these timings; it does not change the measured crawler path and has a
separate PostgreSQL regression.

The full Go race suite ran with disposable PostgreSQL after the reprocessing
implementation. Following the final startup-prefix addition, affected processor,
core processing, protocol and migration race suites passed again. The final
expiry regression, `go vet ./...`, public-content tests and headless/dependency
boundary checks also passed.

Preparation recovery implementation revision:
`9f69694361d105f93e1d23362db3c6c344589180` adds schema 66 and bounded local
preparation retries. It does not change the measured crawler persistence path;
the timings above remain the earlier run, not a new performance measurement.
A real PostgreSQL table lock reproduces the previous bug: a preparation deadline
incorrectly retired unchanged input as stale. Five repeated race-enabled runs
of the corrected preparation, healthy-service, transport-fault and stale-result
tests passed. They verify restart recovery with the same input, finite exhaustion,
cancellation, retained failure evidence and no invented service attempts or
catalogue applications. The normal test fixture uses production request bounds;
the deliberate 150 ms deadline applies only to the HTTP timeout injection,
whose two actual dispatches and failed receipts are asserted separately.
The subsequent complete Go race suite with disposable PostgreSQL passed for
all packages, including the new preparation recovery and migration checks.

The targeted tests cover:

- Complete latest500 selection, version isolation, retention holes, unknown and
  deferred outcomes, expired leases, and a separate complete startup prefix
  that never treats missing history as zero.
- Stable independent Python/Go cohort vectors, exact percentage boundaries,
  default full-cohort compatibility, zero admission, restart recovery, atomic
  generation changes, audit pagination and rejection of stale application.
- Authenticated bounded exports, retained identities after plugin removal,
  explicit raw/reference gaps and refusal to truncate an oversized history.
- Normal-core-first reprocessing, correlated receipts, duplicate reconciliation,
  before-image retention after origin job cleanup, unchanged restrictions,
  current privacy/source/pipeline checks, missing/failed/deferred/expired work,
  cancellation of a live optional lease and operation with zero plugins.
- Actual core processor handling of destructive classifier/filter verdicts:
  audited reprocessing retains torrents and records deferral. Completed ordinary
  processing does not invent a completed unsuccessful match when no search ran.

These results do not qualify any model or authorize deployment. Exact-head
hosted CI, native container/artifact checks, independent consumer integration,
matched-backup recovery and real deployment-window measurements remain required.
