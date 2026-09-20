# BitAgent

<p>
  <a href="LICENSE"><img alt="MIT licence" src="https://img.shields.io/badge/licence-MIT-blue.svg"></a>
  <a href="https://buy.stripe.com/8x26oH2U44f65TRe574wM04"><img alt="Donate" src="https://img.shields.io/badge/donate-Stripe-635bff.svg?logo=stripe&logoColor=white"></a>
</p>

**A self-hosted BitTorrent DHT crawler and indexer built for the \*arr stack.** Sonarr, Radarr, Lidarr, Readarr and Prowlarr talk to it over Torznab; it watches what they actually grab and import, and feeds that ground truth back into classification, ranking and retention. Opt-in LLM stages, an operator console and a public library front-end sit on top of a hardened Go core.

BitAgent started in April 2026 as a fork of [`bitmagnet-io/bitmagnet`](https://github.com/bitmagnet-io/bitmagnet), which has been effectively dormant since July 2025 (one dependency bump since). The DHT crawler, metainfo fetcher, CEL classifier and the Postgres/GraphQL foundation are upstream's work and are credited below. Almost everything else on this page is new: against the fork baseline the tree adds roughly 100K lines of non-test Go, 33 schema migrations and 16 new packages. The diff is public — [compare the baseline with `main`](https://github.com/spencercnorton/bitagent/compare/2b9e8eadd34c037830d1fa7470b5ef2746cd6388...main).

## What BitAgent adds

### Ground truth from your \*arr stack

- **Evidence pipeline.** `POST /evidence/arr/:instance` receives Sonarr/Radarr/Lidarr/Readarr webhooks; a history poller backstops lost deliveries; a qBittorrent poller reads categories and tags. Every signal lands in an append-only `label_evidence` table and resolves, by explicit precedence, into one canonical label per infohash.
- **Canonical preempt.** A label backed by a real grab short-circuits the classifier — nothing to compute. Lookup errors fall through to the rules instead of failing the torrent.
- **Grab-outcome priors.** Beta(α, β) success priors learned per release feature from grab → import outcomes re-rank Torznab results toward what has actually worked for you.
- **Liveness.** qBittorrent state observations and import events derive alive / suspect / dead per infohash; dead swarms are excluded from Torznab, and a revalidator revives them through DHT `get_peers`.
- **Wantbridge.** Every newly discovered name is matched live against what your \*arrs are still missing, with per-source wantlist and match metrics. Today this is a shadow measurement — fetch prioritisation on a match is the next step, and the flag for it (`WANTBRIDGE_ENFORCE`) already exists.
- **Attribution.** `bitagent attribution recon` reconstructs, per infohash, what the indexer served, what the client grabbed and what the \*arr imported — so hit-rate claims are measured, not folklore.

### Curation, not just collection

- **Content filter.** A deterministic ladder — language tag or title script, lossy-audio-only music, NSFW, blocked extensions and content types, foreign audio — in pure Go with no network or database call per decision. Anime-aware, so watchable anime is never dropped on a script heuristic. Shadow mode counts would-drops before anything is enforced.
- **Junk purge.** Persistently unmatched rows are judged by name (see [LLM integration](#llm-integration)); only confident junk moves to a quarantine you can restore from.
- **Retention.** Opt-in purge of rows with no canonical label, no evidence, zero seeders on every source and age past `MinAge`. Dry-run by default.
- **CSAM defence.** A pre-fetch bloom filter of double-hashed infohashes from external feeds is consulted before any BEP-9 fetch — closing the window between first sight in the swarm and post-fetch rejection. Hashes the classifier deletes are exported back. See [`docs/csam-defense.md`](docs/csam-defense.md).
- **Verdict ledger.** An append-only event log plus a derived current verdict per infohash; the blocked set feeds Torznab exclusion and crawler pre-fetch triage.
- **Queue cleaner.** Processed and failed queue rows are purged instead of growing an index larger than the data.

### Titles that actually match

- **Anime backbone.** LLM-free detection of fansub brackets, absolute episode numbers and romaji season markers on the always-on path, plus an alias table built from AniDB titles joined to Anime-Lists TMDB mappings — every romaji, kanji, English and fan alias resolves to one TMDB id.
- **Alternative titles.** TMDB alternative titles and translations are folded into full-text search, so foreign-named releases find their canonical entry.
- **One normaliser.** `internal/titlenorm` is the single title normaliser every matching surface calls; the repo once had eleven.
- **Derived columns.** Release granularity, release date, anime absolute episode and English-audio presence are stamped on every row, with resumable backfills for rows classified before they existed.
- **Parser fixes.** Three-digit episode numbers and concatenated multi-episode names (`S01E01E02`) parse correctly.

### Accurate swarm numbers

- **Tracker scrape.** A BEP-15 UDP scrape worker refreshes seeders and leechers from public trackers — authoritative counts at about seventy infohashes per datagram, replacing the crawler's one-node, never-refreshed bloom estimate.
- **Peer reputation.** Per-peer BEP-9 history stops the fetcher re-hammering peers that never answer; the audit that motivated it found 8.68M errors against 207K successes.
- **DHT security.** BEP-42 external-IP resolution with a validity-based restart watcher, BEP-43 read-only handling on both paths, BEP-51 peer-interval compliance.

### Observability that answers questions

- **`bitagent_*` metric families** for every subsystem — DHT server/client/responder, routing table, crawler, classifier, evidence, content filter, liveness, priors, retention, junk purge, seeds, wantbridge and each LLM stage. See [`docs/reference/metrics.md`](docs/reference/metrics.md).
- **`pgstats`** reports table and index bloat, autovacuum lag, dead tuples and pool saturation on every scrape, so storage drift is visible before it is an incident. **`dashstats`** publishes the dashboard's headline figures as gauges.
- **Grafana dashboards** and Prometheus / Loki / Pyroscope configs ship in [`observability/`](observability/). A `bitmagnet_*` dual-emit shim keeps dashboards built against upstream alive.

### Operations

- **Secrets as files.** Every credential accepts a `_FILE` variant; an empty plain value never shadows the file, and a missing or empty file is a startup error rather than a silently disabled feature.
- **Shadow first, everywhere.** Every destructive or costly feature has a two-flag opt-in (`Enabled`, then `Enforce` / `EnableLive` / `EnablePurge`) and counterfactual `would_*` metrics, so you read the numbers before you flip the switch.
- **Evaluation harness.** `eval-freeze` snapshots corpora into an immutable table, `eval-replay` runs them through the real workflow as byte-stable JSONL, and `matcher-eval` compares two models on the same sample.
- **Backfills for everything** — `derived-backfill`, `episodes-backfill`, `anime-backfill`, `refresh-alt-titles`, `refresh-anime-titles`, `refresh-seeds` — bounded, resumable and dry-run capable.

## Two surfaces, one dashboard

The core image ships no web UI: upstream's Angular SPA was removed at fork time, every non-API path returns 404 and the container healthcheck hits `/metrics`. The dashboard is **`bitagent-ui`**, a separate FastAPI + vanilla-JS service that reads the core's GraphQL and Prometheus endpoints and never mutates the corpus. One app serves two hostnames, and the host you arrive on decides what you see.

**Operator console** (`OPERATOR_HOSTS`) — the administrator's side.

- **Dashboard** led by Indexer Win Rate (30d), with match rate, grab liveness, crawl throughput and indexed count. Every figure arrives in a source-aware envelope: an unavailable value renders as unknown, never as zero, and a rate still filling its baseline says *measuring…*.
- **Library**, **Wants** (wantbridge health per source), **Evidence** (per-source received / persisted counts ahead of the raw event list).
- **Quarantine** — review what the junk judge would remove, restore it or delete it now, and *Spot-check 25* random rows from a hold too large to read through.
- **AI** — one scorecard per LLM stage on shared axes (model, throughput, latency, a quality proxy) and a spend row against a monthly budget. A model missing from the price table turns the total into a floor rather than reading as free; no accuracy figure is invented where no ground truth exists.
- **Settings** (a read-only view of the configuration the container actually sees, secrets redacted) and **System** (health checks, a Torznab tester, a GraphQL explorer, raw metrics).
- Inline ⓘ help on every stat card and column, and a push-to-\*arr action for any release.

**Public library** (`PUBLIC_LIBRARY_HOSTS`) — the end user's side, for the people you share the indexer with.

- Poster browse and search with genre, year, quality and source facets, and title pages that list every release.
- An account panel where each user generates, rotates and revokes a **personal Torznab key**. Keys are stored hashed; the UI's `/torznab/*` proxy validates and rate-limits per key, then forwards to the core with the operator's key. Each user gets their own indexer credential for their own Prowlarr or Sonarr and never sees the core's.

Authentication is layered — API key (constant-time compare), NPM `auth_request` headers, or a generic `X-Forwarded-User` — and the proxy tiers require both a trusted transport-peer CIDR and a shared proof header. There is deliberately no cookie-presence tier. Hostname selects the surface; only a verified role header grants operator authority, and unknown hosts get `421`. `bitagent-ui` ships separately from this repository; its surfaces are documented in [`docs/ui-guide.md`](docs/ui-guide.md), [`docs/system-tab.md`](docs/system-tab.md) and [`docs/reference/dashboard-api.md`](docs/reference/dashboard-api.md).

## LLM integration

Four points in the pipeline can ask a model. All four are **off by default** — without them BitAgent is a fully deterministic indexer — and each speaks to an OpenAI-compatible chat endpoint, hosted or self-hosted (Ollama, vLLM), so the model can live on your own LAN and nothing leaves the house. The matcher can also route through OpenRouter, pinned to one provider with zero-data-retention and no fallback.

| Stage | Runs when | Decides |
|---|---|---|
| **TMDB matcher** (`internal/classifier/llmmatch`) | The rules typed a torrent as movie or TV but neither local search nor TMDB search could attach an id — the releases your \*arrs cannot grab | Two calls: *extract* `{title, year, type, season, episode}` from a mangled name, then *rerank* the TMDB candidates. Told to return nothing when unsure: a wrong match is worse than none |
| **Type fallback** (`internal/classifier/llmstage`) | The rules returned `ErrUnmatched` | `{category, confidence}` from the title, a hash of the file list and a size bucket — never paths or exact sizes |
| **Content-filter tier** (`internal/classifier/contentfilter`) | The residual the deterministic ladder cannot decide | Keep or drop, with confidence. A rule miner turns recurring verdicts into candidate deterministic rules so the model works itself out of a job |
| **Junk judge** (`internal/junkpurge`) | Movie/TV rows still unmatched after `MinAge` | *Real but mangled* versus *junk*, judged on the name alone so a TMDB outage cannot corrupt the verdict. Only confident junk moves to quarantine |

Guardrails are the same across stages:

- **Shadow before live.** `Enabled` runs the stage and emits metrics; `EnableLive` or `EnablePurge` is a separate switch, thrown only after the shadow numbers clear a bar.
- **Budgets that survive restarts.** Daily and monthly call allowances persist in Postgres (`llm_request_budgets`); every attempt, including failures, consumes a slot, and zero means stop — never unlimited. Request bytes, output tokens, concurrency and timeouts are capped per stage.
- **Privacy gates.** Anything that came from a private tracker — by qBittorrent category or evidence flag — never leaves the host. Inputs are minimal by design, and private infohashes fail closed on a lookup error.
- **Plausibility before spend.** Size, file-count and extension gates reject payloads not worth an inference; sha256-keyed LRU caches on `(model, prompt version, inputs)` make repeats free.
- **Circuit breakers.** The junk judge aborts a cycle without deleting when the provider is unavailable or when an implausible share of a batch comes back as junk — a degraded prompt, not a suddenly junk library.
- **Calls are captured.** Requests and responses are recorded for offline scoring, corpora are frozen and replayed byte-for-byte, and the AI tab shows each stage's model, throughput, latency, quality proxy and spend.

## What's in the box

| Area | Surface |
|---|---|
| Engine | DHT crawler (BEP-5/9/10/33/42/43/51), metainfo requester with peer reputation, CEL classifier, GORM-backed Postgres persistence |
| External API | `/graphql` (GraphQL), `/torznab/*any` (Torznab with liveness exclusion and priors re-ranking), `/import` |
| Evidence | `POST /evidence/arr/:instance` webhook, \*arr history poller, qBittorrent poller → `label_evidence`, `torrent_canonical_labels`, liveness, priors |
| Classification | canonical preempt → CEL rules → optional LLM fallback; anime backbone; alternative titles; one title normaliser |
| Curation | content filter, junk purge with quarantine, retention, CSAM pre-fetch blocklist, verdict ledger, queue cleaner |
| LLM stages | TMDB matcher, type fallback, content-filter tier, junk judge — all opt-in, budgeted, shadow-first |
| Swarm data | BEP-15 tracker scrape, seeds history, revalidation via DHT |
| Metrics | `bitagent_*` Prometheus families, `pgstats`, `dashstats`, Grafana dashboards in `observability/`, legacy `bitmagnet_*` dual-emit |
| CLI | `worker`, `classifier`, `reprocess`, `attribution`, `eval-freeze` / `eval-replay` / `matcher-eval` / `batch-llm-match`, backfills — see [`docs/reference/cli.md`](docs/reference/cli.md) |

## Quickstart

[`examples/README.md`](examples/README.md) has a two-service stack (bitagent + Postgres) that builds from this tree with no operator-specific infrastructure:

```sh
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent
cp examples/.env.example examples/.env.public     # set POSTGRES_PASSWORD at minimum
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build
curl -s http://localhost:3333/metrics | head -5    # GraphQL playground: http://localhost:3333/graphql
```

Then add `http://<host>:3333/torznab` as a Torznab indexer in Prowlarr or directly in each \*arr — per-app guides are under [`docs/integrations/`](docs/integrations/sonarr.md) — and point the \*arr's webhook at `/evidence/arr/<instance>` so the evidence loop closes ([`docs/evidence.md`](docs/evidence.md)).

The maintainer's own deployment (VPN egress, private ingestors, monitoring) is a separate operator playbook and is not part of this repository.

## Documentation

- [Docs index](docs/index.md) · [Quickstart](docs/quickstart.md) · [Configuration](docs/configuration.md) · [FAQ](docs/faq.md) · [Troubleshooting](docs/troubleshooting.md)
- Concepts: [Architecture](docs/concepts/architecture.md) · [DHT crawler](docs/concepts/dht-crawler.md) · [Classification](docs/concepts/classification.md) · [Wantbridge](docs/concepts/wantbridge.md) · [Glossary](docs/concepts/glossary.md)
- Reference: [Torznab API](docs/reference/torznab-api.md) · [GraphQL API](docs/reference/graphql-api.md) · [Dashboard API](docs/reference/dashboard-api.md) · [Metrics](docs/reference/metrics.md) · [CLI](docs/reference/cli.md)
- Operations: [Security](docs/operations/security.md) · [Monitoring](docs/operations/monitoring.md) · [Private tracker mode](docs/integrations/private-tracker-mode.md) · [CSAM defence](docs/csam-defense.md)
- Project: [Improvements over upstream](docs/project/improvements.md) · [Legal disclaimer](docs/legal/disclaimer.md)

## Development

```sh
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent

go test ./...                        # vanilla suite, CGO off
CGO_ENABLED=1 go test -race ./...    # race-detector suite
go build .                           # produces ./bitagent

# run against a real Postgres:
POSTGRES_HOST=localhost POSTGRES_PASSWORD=bitagent ./bitagent worker run --all
```

Contributions are welcome — read [CONTRIBUTING.md](CONTRIBUTING.md) first. This repository is a release mirror: pull requests are reviewed here and ship in the next tagged release.

## Support

- Bugs and feature requests: [open an issue](https://github.com/spencercnorton/bitagent/issues/new/choose). Questions: [Discussions](https://github.com/spencercnorton/bitagent/discussions).
- Security: see [SECURITY.md](SECURITY.md).
- If BitAgent earns a place in your stack, you can [support its development](https://buy.stripe.com/8x26oH2U44f65TRe574wM04).

## Lineage & licence

Licensed under the MIT licence, inherited from [`bitmagnet-io/bitmagnet`](https://github.com/bitmagnet-io/bitmagnet). All upstream copyright notices are preserved, and the parts that are still upstream's — the DHT protocol implementation, metainfo fetching, the CEL classifier engine and the GraphQL/Postgres foundation — are the reason the rest was possible.

BitAgent's baseline is upstream commit `2b9e8ea`, the head of upstream `main` from July 2025 until May 2026. The fork was cut in April 2026 and stopped tracking upstream on a schedule at the rebrand that month; the `upstream` remote stays configured for security-fix surveillance.
