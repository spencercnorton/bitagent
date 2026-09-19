# BitAgent

A headless self-hosted BitTorrent DHT crawler, content indexer, torznab adapter, and *arr-ecosystem evidence pipeline. Started as a fork of [`bitmagnet-io/bitmagnet`](https://github.com/bitmagnet-io/bitmagnet) on 2026-04-20; rebranded to **BitAgent** on 2026-04-24 once upstream dormancy (no `main` commit since 2025-07-01) made continued "fork" framing inaccurate.

BitAgent is a single-purpose tool: stay accurate and non-rotting over years of operation alongside qBittorrent and the *arr stack. It trades away the generic self-hosted UI that upstream ships for a tighter integration with the *arr ecosystem and an agent-style classification pipeline (authoritative labels from *arr webhooks + qBittorrent categories preempt a CEL classifier, which falls through to an optional LLM stage).

## What's in the box

| Area | Surface |
|---|---|
| Engine | DHT crawler (BEP-5/9/10/33/42/43/51 compliant), metainfo requester, CEL-based classifier, GORM-backed Postgres persistence |
| External API | `/graphql` (GraphQL), `/torznab/*any` (torznab) |
| Evidence ingestor | `POST /evidence/arr/:instance` (shared-secret webhook from Sonarr/Radarr), qBittorrent category/tag poller, *arr history poller → `label_evidence` + `torrent_canonical_labels` tables |
| Classifier preempt | canonical labels short-circuit the CEL runner — the evidence pipeline is the primary classification path, CEL is fallback |
| Content filter | deterministic ladder (non-Latin script, lossy-audio-only, NSFW, blocked extensions/types) + optional LLM tier on the residual cohort. The LLM stage is OpenAI-compatible (api.openai.com or a self-hosted Ollama/vLLM endpoint), bounded by a per-UTC-day call budget, with an LRU verdict cache and a rule-miner that surfaces recurring verdicts as candidate deterministic rules |
| LLM fallback | shadow-mode-first stage after CEL `ErrUnmatched`, privacy-gated on source tracker |
| Retention | opt-in purge (`Enabled`/`EnablePurge` both default off): no canonical label + no evidence + age >= `MinAge` + all sources zero-seeders. When enabled it live-purges on a schedule, so corpus growth stays bounded |
| Junk purge | optional, off-by-default guarded worker (`internal/junkpurge`): an LLM judges persistently-unmatchable movie/tv torrents by name and deletes only confident-junk, behind two circuit breakers and a two-step enable (dry-run → enable-purge) |
| DHT security | BEP-42 runtime external-IP resolver + validity-based restart watcher; BEP-43 read-only handling on inbound + outbound paths; BEP-51 peer-interval compliance |
| Metrics | `bitagent_*` Prometheus namespace — DHT server/client/responder, ktable, crawler, evidence, classifier, content-filter, liveness, peer-rep, retention, junkpurge, pgstats (Postgres health). A legacy `bitmagnet_*` dual-emit shim from the rebrand is still present for back-compat |

No embedded web UI in the core image — the Angular SPA was stripped at fork time, and every non-API path (including `/`) returns Go's `404 page not found` by design. The container healthcheck hits `/metrics`. The operator dashboard is a **separate** FastAPI app (`bitagent-ui`) that reads the core's GraphQL API; programmatic consumers can also use GraphQL or the Torznab adapter directly.

## Deploy

For self-hosters: see [`examples/README.md`](examples/README.md) — a two-service quickstart (bitagent + postgres) that builds from local source, no operator-specific infra. `cp examples/.env.example examples/.env.public` → set `POSTGRES_PASSWORD` → `docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build` → `http://localhost:3333/graphql`.

The maintainer's own deployment runs behind a VPN-attached Portainer stack from a git-backed compose file; that operator playbook is not part of this repository.

## Development

```sh
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent

go test ./...          # vanilla suite, CGO off
CGO_ENABLED=1 go test -race ./...   # race-detector suite (CI job: test-race)
go build .             # produces ./bitagent

# run against a real Postgres:
POSTGRES_HOST=localhost POSTGRES_PASSWORD=bitagent ./bitagent worker run --all
```

Standard workflow: branch → MR → Jeeves green → merge → Portainer auto-redeploys.

## Lineage & license

Licensed under the MIT license, inherited from the upstream project (`bitmagnet-io/bitmagnet`). All upstream copyright notices are preserved.

BitAgent stopped tracking upstream on a schedule at the 2026-04-24 rebrand. The `upstream` git remote (`github.com/bitmagnet-io/bitmagnet`) stays configured for reference (security-fix surveillance) but BitAgent is no longer tracking upstream on a schedule.
