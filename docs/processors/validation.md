# Validation of the processor boundary

Date: 2026-10-16. Baseline is `36de3b92d1169207b043f12244c16502b2de0a27`
(PR #67). The implementation runtime checkpoint is
`d77f19a98290222e99264eeafa16075bd39f1f68`; subsequent validation-stage changes
add the process-crash assertion, isolate the migration-60 regression fixture,
and publish this report. Measurements use synthetic inputs, Go 1.26.9,
macOS/arm64 on Apple M4 Pro, and disposable PostgreSQL 16 in Docker.
Raw numeric samples and provenance are in [validation.json](validation.json).

## Baseline comparison

The same two harnesses run against baseline and candidate. The deterministic
benchmark invokes the actual CEL workflow and parser with movie, TV and music
names; external metadata resolution and optional inference are disabled on both.
The crawler test invokes `runPersistTorrents`, including the actual metadata,
piece, source and PostgreSQL queue transaction. Only network acquisition and
subsequent tracker scraping are replaced by synthetic input/output channels.
Schema setup is outside the timed interval.

| Measurement | Baseline | Candidate | Interpretation |
| --- | ---: | ---: | --- |
| Deterministic processing median, five runs | 912,991 ns/op | 885,069 ns/op | 3.1% lower elapsed time in this local sample |
| Median allocations per operation | 3,284 | 3,232 | 1.6% fewer allocations |
| Crawler persistence median, ten samples/version | 9,414.7 torrents/s | 9,579.6 torrents/s | +1.8%; smaller than the observed run variation |
| Crawler persistence range | 8,137.8–10,301.5/s | 8,764.4–11,291.4/s | Overlapping ranges; not evidence of a production speed improvement |
| Rows per crawler run | 25,000 | 25,000 | All torrents, piece rows and source links retained |
| Durable core queue jobs per run | 250 | 250 | All expected batches queued |

Crawler comparison uses ABBA order, GOMAXPROCS=2, five batches of 5,000 per run,
and no concurrent database tests. An initial uncontrolled pair gave medians
9,136.9/s and 8,168.2/s; that variance motivated the controlled interleaving.
Do not cherry-pick either observation as an estate throughput claim. Discovery
and BEP-9 algorithms are unchanged; actual DHT yield depends on network state,
warmup and peer availability and still requires deployment-window measurement.

Reproduce the deterministic sample with:

```sh
go test -run '^$' -bench BenchmarkDeterministicPipeline -benchmem -count=5 ./internal/classifier
```

Copy the identical `pipeline_benchmark_test.go` and
`persistence_measurement_test.go` into a detached baseline checkout, then run:

```sh
GOMAXPROCS=2 BITAGENT_TEST_POSTGRES_DSN='<disposable database DSN>' \
  go test -count=1 -v -run '^TestMeasuredCrawlerPersistence$' ./internal/dhtcrawler
```

Run baseline, candidate, candidate, baseline serially. Each invocation asserts
all four table populations after timing; a failed assertion invalidates the run.

## Functional and failure acceptance

| Case | Evidence and observed behavior |
| --- | --- |
| Zero plugins | A real backend binary migrates an empty database to 61, starts `http_server`, `queue_server`, and `processors`, reports ready with zero registrations, and serves GraphQL, Torznab capabilities and Prometheus. No model/provider settings are needed. |
| Two independent implementations | Real Go reference and Python standard-library service processes complete classification/policy jobs through PostgreSQL. Authenticated status, counters and filtered cursor pagination reconcile to executed attempts. |
| Actual process crash | SIGKILL of the Python service after registration produces two bounded technical failures. New enrichment is withheld, an existing movie remains intact, and core readiness remains ready. |
| Timeout and unavailable endpoint | Finite deadlines and attempts; no junk verdict, deletion or publication grant. Other queue work and existing facts survive. |
| Hostile responses | Duplicate keys, unknown fields, oversized bodies, wrong content type, redirects and incompatible bindings/contracts are rejected. |
| Duplicate delivery and stale input | Repeated completion loses its lease CAS; source changes, cancellation, replacement and obsolete versions reject application. |
| Core restart recovery | Expired leases are reclaimed with a larger generation; prepared input/candidates/results remain frozen and old completion is rejected. |
| Ordering and backpressure | Stage/priority/ID ordering is stable; conflicting type proposals follow precedence. Admission, leased concurrency, retries and retained history remain bounded. |
| Source identity and catalogue integrity | A catalogue identity introduced during inference makes an otherwise matching candidate ambiguous and prevents attachment. Accepted exact identity retains search aliases. Existing hints/types/identities and privacy authority prevail. |
| Policy exclusion | Actual serving SQL excludes the row after plugin removal, while the catalogue row remains present. No automatic clearing on outage, rollback or removal. |
| Legacy migration and recovery | Queued/deferred/leased jobs retain complete before-images and become held. Completed historical applications survive. A snapshot captured before migration 61 restores afterward. Evidence-bearing downgrade is refused. |
| Independent service upgrade and rollback | Two separately running compatible A/B services use the same core process and Store: generation 1 → 2 → 3, old result rejected, movie → TV → movie for separate synthetic inputs. Removal preserves four typed rows including an existing match. |
| Actual indexer responses | Real Torznab HTTP/XML returns two movie/category-2000 and one TV/category-5000 synthetic releases, invents no identifiers, and retains pre-existing `tmdbid=991001` through upgrade/rollback/removal. |
| Credential rotation | Atomic replacement of a dedicated service-token file changes authentication without core restart; path traversal and credential-bearing endpoint syntax fail validation. |

The external A/B test is generic and opt-in via `PROCESSOR_TEST_ENDPOINT_A`,
`PROCESSOR_TEST_ENDPOINT_B` and `PROCESSOR_TEST_TOKEN_FILE`. An independent
integration task reproduced contract, supported installer and core/Torznab
acceptance against a clean checkout of the runtime checkpoint. This establishes
protocol/integration behavior, not model accuracy or qualification.

## Regression and distribution checks

Baseline full race tests passed (87 tested packages); the candidate full race
suite passes (79 tested packages). The difference includes retired embedded
model packages; their source and historical evidence remain preserved.
`go vet ./...`, the headless/processor dependency gates and public-content checks
pass. The called-dependency scan reports zero reachable vulnerabilities. The
complete reachable Git history passes the secret scanner.

The PostgreSQL suite exercises crawler persistence, quarantine, processors,
deterministic filtering, historical captures/work receipts, search, serving,
seeds, normal processing, name policy, importing, release fields, catalogue
policy and complete recovery. Adding migration 61 exposed an old migration-60
test's assumption that 60 was latest; the fixture now explicitly starts at 60
before its down/up drill, preserving its catalogue and recovery assertions.
The new migration does not edit historical migrations or the recoverable
catalogue cascade schema.

Public CI builds the supported `linux/amd64` image and checks it with networking
disabled and a read-only root filesystem. The worker registry includes core and
optional processors and excludes retired inference workers. No Python, Node,
Go toolchain or site runtime is shipped. `_legacy` is excluded from the Docker
build context. The main executable's dependency graph contains no embedded
inference, capture runtime or legacy model-work dispatcher.

## Limits and release gates

These measurements are local synthetic qualification. They do not establish
live-network crawl yield, large-catalogue latency, plugin model quality or a
production deployment. The short final application transaction has a 500 ms
budget and takes catalogue/authority locks without network I/O; measure its
contention under representative production load before enabling apply mode.
Service CPU/RAM/PID/network limits belong to the external supervisor; the
example Compose demonstrates them and the core enforces transport/work bounds.

Follow [migration and rollback](migration.md), retain a matched database backup,
qualify the immutable released image, and compare equivalent warm crawl windows
before production promotion. Core rollback with evidence requires a matched
backup; it is not a destructive schema downgrade. UI and individual plugin
packaging/qualification remain separate deliverables.
