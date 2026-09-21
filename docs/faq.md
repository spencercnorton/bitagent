# Frequently Asked Questions

## What BitAgent is

### Q: How is BitAgent different from bitmagnet?

A: BitAgent is a hardened, indexer-focused fork of bitmagnet engineered specifically for the *arr ecosystem integration pipeline. It strips unnecessary daemon layers, replaces the default SQL backend with optimized PostgreSQL schemas, and ships with strict Torznab compliance by default. Unlike upstream, it enforces rate limiting and exposes structured evidence fields directly to indexer clients. See [concepts/architecture.md](concepts/architecture.md) for the full component comparison.

### Q: How does it compare to Prowlarr indexers?

A: Prowlarr aggregates external HTTP tracker APIs, while BitAgent crawls the DHT network directly to build a self-contained torrent index. It operates without subscription walls or API key dependencies, relying entirely on local peer discovery and BEP routing. You run it as a standalone HTTP service that exposes a compliant Torznab endpoint for *arr clients.

### Q: Can BitAgent replace Prowlarr entirely?

A: No. BitAgent complements Prowlarr rather than replacing it. You should configure BitAgent as a secondary indexer to supplement your existing tracker list with high-entropy DHT results. Prowlarr still manages your primary HTTP-based indexers, while BitAgent fills gaps in the swarm.

### Q: What protocols does BitAgent actually index?

A: BitAgent indexes torrents it discovers on the mainline DHT and fetches their metadata from peers over BEP-9 (`ut_metadata`). From that it extracts size, file lists and names; seeder/leecher counts come from the optional `seeds` worker, which scrapes public trackers over BEP-15. It does not index Usenet, HTTP trackers, or direct download links out of the box.

### Q: How does it differ from standard DHT crawlers?

A: Generic DHT crawlers dump raw k-bucket entries and lack parsing logic, whereas BitAgent runs a full classifier pipeline that normalizes releases into structured evidence objects. It deduplicates via InfoHash, maps custom formats automatically, and persists data in a query-optimized store. This transforms raw swarm noise into *arr-ready results.

## Setup + deployment

### Q: What are the Docker Compose prerequisites?

A: You need Docker with the Compose v2 plugin (`docker compose version`), a Postgres 14+ instance (the bundled `postgres:16` service in `examples/docker-compose.public.yml` works with zero configuration), and a minimum of 2 vCPUs and 512MB RAM allocated to the service. There are no host directories to pre-create: the reference stack uses named volumes. See [quickstart.md](quickstart.md).

### Q: How do I initialize the Postgres database?

A: BitAgent auto-migrates tables on first boot if the schema is missing. Point it at the database with `POSTGRES_HOST`, `POSTGRES_PORT` (`5432`), `POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_NAME` (default `bitmagnet`, kept for compatibility with pre-rebrand deployments), or pass one `POSTGRES_DSN` instead. For external Postgres, create the database and user first and verify connectivity with `pg_isready -h <host> -U <user>` before starting.

### Q: How should persistent volumes be configured?

A: Postgres is the store, so the volume that matters is the database's `/var/lib/postgresql/data`. The core itself keeps only optional small state under `/root/.config/bitmagnet` (a `config.yml`, if you use one) and `/root/.local/share/bitmagnet` (the optional log rotator), and the dashboard keeps its SQLite file under `/data`. The reference compose file mounts all four as named volumes; there is no data-directory environment variable to set.

### Q: What network mode is required for DHT?

A: None beyond outbound UDP. The DHT server binds UDP `3334` on all interfaces (`DHT_SERVER_PORT`); the reference compose file publishes `3334/tcp` and `3334/udp`. Traffic is outbound-dominant, so the crawler still works behind NAT without a port forward — throughput is just lower. `network_mode: host` is not required.

## Auth + security

### Q: What authentication modes are supported?

A: Two surfaces, two answers. The core's HTTP API has no login of its own: `/torznab` is gated by `TORZNAB_API_KEY` and `/evidence/arr/*` by an evidence token; everything else trusts whoever connects, so put a reverse proxy in front before exposing it. The dashboard (the `ui` worker) resolves identity through three tiers: `DASHBOARD_API_KEY` (`?apikey=`, `X-Api-Key` or `Authorization: Bearer`), then `X-Auth-User-Id` or `X-Forwarded-User` injected by a reverse proxy that performed the login — the proxy tiers require both a trusted peer CIDR and a proof header. `REQUIRE_AUTH=false` opens everything and is for development only. See [ui/README.md](../ui/README.md) → Authentication.

### Q: Why is apikey the default auth method?

A: API keys provide stateless, low-overhead validation ideal for *arr indexer clients. They avoid session cookie parsing, reduce attack surface, and integrate natively with Sonarr/Radarr/Prowlarr indexer tokens. The key is validated on every Torznab request (`apikey=` query parameter or `X-Api-Key` header) with a constant-time compare.

### Q: What does the threat model cover?

A: The model assumes untrusted *arr clients on internal networks and focuses on API endpoint hardening and rate limiting. It mitigates credential stuffing via rate limiting and prevents path traversal via strict input sanitization. It does not cover host kernel vulnerabilities or DHT network-level eavesdropping.

### Q: How do I rotate API keys safely?

A: `TORZNAB_API_KEY` and `DASHBOARD_API_KEY` are environment variables: change the value, recreate the container, then update the *arr indexer configuration (or your scripts) with the new key. There is no dual-key window; do it in a maintenance moment. Users of the public library rotate their own personal Torznab keys from the account panel without touching the core's key.

### Q: What reverse proxy guidance applies?

A: Place BitAgent behind Caddy, Traefik, or Nginx with `proxy_set_header X-Real-IP $remote_addr` enabled. If exposing to the public internet, set `TRUST_NPM_HEADERS=false` so that client-supplied `X-Auth-User-Id` headers cannot bypass the API key. Always terminate TLS at the proxy.

## Sonarr/Radarr integration

### Q: What Torznab URL format is required?

A: Configure your indexer as `http://bitagent.example.com:3333/torznab` in the *arr dashboard. Append `apikey=<TORZNAB_API_KEY>` or set the API Key field. Ensure the protocol matches your container port mapping and DNS resolution.

### Q: How do I map evidence to custom formats?

A: BitAgent returns structured `<info>` attributes containing `resolution`, `hdr`, `audio_channels`, and `encoder` fields that *arr can match via custom format IDs. Align the custom format IDs with your library standards; the field names are documented in the [Torznab API reference](reference/torznab-api.md).

### Q: How do I fix search timeouts in *arr?

A: There is no search-timeout setting in BitAgent; the ceiling is the *arr client's own indexer timeout and Postgres `statement_timeout`. The query cache is on by default (`GORM_CACHE_CACHE_ENABLED=true`, `GORM_CACHE_TTL` `10m`, `GORM_CACHE_MAX_KEYS` `1000`) so repeated searches are cheap; if searches are slow, look at `bitagent_postgres_*` on `/metrics` and tune Postgres (see [operations/performance.md](operations/performance.md)).

### Q: How do I set up the evidence webhook?

A: It is inbound, not outbound: BitAgent does not push releases anywhere. Set `EVIDENCE_WEBHOOK_SECRET`, then add a Connect → Webhook in each *arr pointing at `http://<bitagent>:3333/evidence/arr/<instance>` (for example `/evidence/arr/sonarr`) with the same value in an `X-Evidence-Token` custom header. Grab/import/failure events then feed the evidence pipeline; see [evidence.md](evidence.md).

### Q: Should BitAgent be primary or fallback?

A: BitAgent's DHT coverage is broad but less curated than private trackers, so fallback placement (priority `50–60`) prevents false positives from dominating. Use `Fallback to` settings to trigger BitAgent only when higher-priority indexers return empty. For dedicated DHT-only setups, set BitAgent as primary.

## Resource usage

### Q: What are the baseline resources for 100k torrents?

A: Allocate 2 vCPUs, 512MB RAM, and 40GB NVMe disk for a stable 100k-entry index. Expect ~0.8% CPU usage during idle crawls and peak 1.2 cores during classifier runs. SSD latency under 5ms is mandatory for WAL replay.

### Q: How does library growth scale resources?

A: RAM scales linearly at ~4MB per 10k entries for the in-memory LRU cache. Disk grows ~1.5GB per 10k entries due to evidence cache and Postgres TOAST bloat. Add storage before hitting 80% inode usage or 4GB heap pressure.

### Q: When should I scale horizontally?

A: Prefer scaling vertically. There is no read-replica setting; every worker talks to the one Postgres, and the classifier assumes a single writer. If the HTTP surface is the bottleneck, run a second container with `worker run --keys http_server` against the same database and load-balance those.

### Q: What are slow-disk gotchas?

A: HDDs cause WAL writer stalls, triggering DB lock-timeout errors during heavy classification. Monitor `pg_stat_io` for await times >10ms and switch to block-backed storage.

### Q: How do I monitor heap and GC?

A: Scrape `/metrics` on the HTTP port (`HTTP_SERVER_LOCAL_ADDRESS`, default `:3333`) with Prometheus. Watch `go_memstats_alloc_bytes` and `gc_pause_ns` for collection latency spikes. Alert when heap growth exceeds 200MB/hr or GC pauses breach 50ms.

## Privacy

### Q: Does BitAgent expose my IP on DHT?

A: Yes. The DHT protocol requires your node to announce its IP via BEP-05 and BEP-10 for routing table population. You cannot route DHT traffic through a proxy without breaking k-bucket consensus. Use a dedicated VM, network VLAN, or route through a VPN to isolate exposure.

### Q: Are TMDB queries cached or leaked?

A: Two clients, two answers. The core's classifier (`queue_server` worker) calls `https://api.themoviedb.org/3` for every movie/TV lookup and stores the result in Postgres; there is no TMDB cache in the core, and `TMDB_ENABLED=false` is the only thing that stops the calls. Leaving `TMDB_API_KEY` empty does **not** disable it — the core falls back to bitmagnet's built-in shared key at 1 request/second and logs a warning. The dashboard (`ui` worker) has its own client that only runs when its `TMDB_API_KEY` is set; it caches posters and metadata in its SQLite file (`poster_cache`, `tmdb_meta_cache`, 7-day TTL), so repeat views do not hit TMDB.

### Q: What data leaves my host externally?

A: There is no telemetry, usage reporting or update check anywhere in the binary. With the defaults (`worker run --all`) the core contacts:

- **External-IP echo** — `GET https://api4.ipify.org`, `https://ipv4.icanhazip.com` and `https://ifconfig.me/ip` to derive a BEP-42 node ID when the DHT stack starts (the `dht_crawler` worker, or `evidence_liveness_revalidator` when `EVIDENCE_LIVENESS_ENABLED=true`), re-polled every 15 minutes (`DHT_EXTERNALIP_INTERVAL`). `DHT_EXTERNALIP_OVERRIDE=<your public IPv4>` replaces it with a static value and makes no HTTP call.
- **DHT and peers** (`dht_crawler` worker) — UDP on `DHT_SERVER_PORT` (default `3334`) to the bootstrap routers (`router.bittorrent.com`, `router.utorrent.com`, `dht.transmissionbt.com`, `dht.aelitis.com`, `router.silotis.us`, `dht.libtorrent.org`; override `DHT_CRAWLER_BOOTSTRAP_NODES`) and to every peer it discovers, plus TCP to peers for BEP-9 metadata. Your IP and the info-hashes you query are visible to those peers.
- **TMDB** — `https://api.themoviedb.org/3` (`TMDB_BASE_URL`) from the classifier for every movie/TV lookup, and a `/authentication` probe every 5 minutes while `http_server` runs. On by default with a built-in shared key; `TMDB_ENABLED=false` turns it off.

Everything else is off until you opt in, and each has its own switch and endpoint:

- **LLM stages** — four independent OpenAI-compatible clients that send torrent names and metadata to their endpoint: `CLASSIFIER_LLM_ENABLED` → `CLASSIFIER_LLM_ENDPOINT` (default `https://api.openai.com/v1/chat/completions`); `CONTENT_FILTER_LLM_ENABLED` → `CONTENT_FILTER_LLM_BASE_URL` (empty = `https://api.openai.com/v1`); `CLASSIFIER_LLM_MATCH_ENABLED` → `CLASSIFIER_LLM_MATCH_ENDPOINT` (default `http://127.0.0.1:11434/v1/chat/completions`, a local Ollama); `JUNKPURGE_ENABLED` → `JUNKPURGE_LLM_BASE_URL` (default `http://127.0.0.1:11434/v1`).
- **Tracker scrape** (`seeds` worker, `SEEDS_ENABLED`; also `bitagent refresh-seeds`) — BEP-15 UDP scrapes of the public trackers in `SEEDS_TRACKER_URLS` (opentrackr, open.demonii, openbittorrent, …).
- **Anime title refresh** (`anime-titles` worker, `ANIME_TITLES_ENABLED`; also `bitagent refresh-anime-titles`) — downloads `anime-list-full.xml` from `raw.githubusercontent.com/Anime-Lists` and `anime-titles.dat.gz` from `anidb.net`.
- **CSAM blocklist** — feeds listed in `CSAM_BLOCKLIST_FEED_URLS` are fetched at start and every 6 hours; if `CSAM_BLOCKLIST_EXPORT_UPSTREAM_URL` is set, double-hashed observations are POSTed there. Both are empty by default.
- **Your own *arr, qBittorrent and Prowlarr** — the evidence pollers, `wantbridge` and attribution call only the base URLs you configure (`EVIDENCE_SONARR_BASE_URL`, `WANTBRIDGE_SONARR_BASE_URL`, …).

### Q: How long are logs retained?

A: As long as your container runtime keeps them. The binary writes every line to stdout (console format; `LOG_JSON=true` for JSON, `LOG_LEVEL` default `info`) and never rotates or deletes anything, so retention is the Docker log driver's job — set `logging: options: max-size / max-file` on the service (the reference compose file uses `max-size: 50m`, `max-file: 5`). If you want files from the binary itself, `LOG_FILE_ROTATOR_ENABLED=true` additionally writes JSON to `LOG_FILE_ROTATOR_PATH` (default `$XDG_DATA_HOME/bitmagnet/logs`) as `bitmagnet.<timestamp>.log`, starting a new file every `LOG_FILE_ROTATOR_MAX_AGE` (default 1h) or `LOG_FILE_ROTATOR_MAX_SIZE` (default 100 MB) and keeping `LOG_FILE_ROTATOR_MAX_BACKUPS` (default 5) older files. Postgres WAL retention is Postgres configuration, not BitAgent's.

### Q: How do I run in fully-offline mode?

A: There is no `--offline` flag; you choose which workers start. Run `bitagent worker run --keys http_server,queue_server` (omit `dht_crawler` — and `evidence_liveness_revalidator` if you set `EVIDENCE_LIVENESS_ENABLED=true` — the only workers that open the DHT socket, talk to peers and run the external-IP echo) and set `TMDB_ENABLED=false` (an empty `TMDB_API_KEY` is not enough — it falls back to the built-in key). If you do run `dht_crawler` on a box without egress, `DHT_EXTERNALIP_OVERRIDE=<a public IPv4>` replaces the external-IP echo. Leave the opt-in networked features off (`SEEDS_ENABLED`, `ANIME_TITLES_ENABLED`, the four `*_LLM_*` switches, `CSAM_BLOCKLIST_FEED_URLS`, `CSAM_BLOCKLIST_EXPORT_UPSTREAM_URL`, and any *arr/qBittorrent base URLs). Torznab and GraphQL keep serving whatever is already in Postgres; `bitagent worker list` prints every worker key.

## Troubleshooting

### Q: Why are search results empty?

A: Verify the crawler is running and persisting: `curl -s http://localhost:3333/metrics | grep bitagent_dht_crawler_persisted_total` should climb over the first minutes, and the dashboard's DHT PEERS tile should be non-zero. If it stays at zero, check outbound UDP egress and see [troubleshooting.md](troubleshooting.md) → "DHT PEERS = 0". If torrents are persisted but unlabelled, the classifier (`queue_server` worker) is not running — check `worker list`.

### Q: Why is evidence missing classifier tags?

A: No CEL rule matched (`ErrUnmatched`), so the torrent was persisted without a content-type label. Run with `LOG_LEVEL=debug` to see the classifier's decision per torrent, and `bitagent reprocess` to re-run classification over already-indexed torrents after a rule or model change. See [concepts/classification.md](concepts/classification.md).

### Q: How do I fix BEP-9 choking?

A: Lower the crawler's footprint: `DHT_CRAWLER_SCALING_FACTOR` (default `10`) scales every internal concurrency and buffer, and `DHT_CRAWLER_METAINFO_CONCURRENCY` (default `0` = 40 × scaling factor) caps concurrent BEP-9 fetches directly. Outbound query rate per peer is bounded by `DHT_SERVER_QUERY_LIMITER_RATE_PER_SEC` (`1.0`) and `DHT_SERVER_QUERY_LIMITER_BURST` (`16`). Watch `bitagent_dht_*` on `/metrics` and reduce if error rates climb.

### Q: Why do *arr clients return 401/403?

A: The indexer token does not match `TORZNAB_API_KEY` or the route is blocked. Verify the *arr indexer's API Key field matches exactly — BitAgent reads it from the `apikey=` query parameter or an `X-Api-Key` header. Run with `LOG_LEVEL=debug` to inspect request validation.

### Q: How do I recover from Postgres index corruption?

A: Stop the writers first — restart with `worker run --keys http_server` so only reads continue — then `REINDEX` the affected indexes in Postgres. Restore from a backup if corruption persists; see [operations/backup-restore.md](operations/backup-restore.md).

## Performance tuning

### Q: How do I tune Postgres autovacuum?

A: Set `autovacuum_vacuum_scale_factor=0.05`, `autovacuum_analyze_scale_factor=0.02`, and `autovacuum_max_workers=3` for high-churn classifier tables. Increase `maintenance_work_mem=512MB` to accelerate index builds. Monitor `pg_stat_user_tables.n_dead_tup`.

### Q: What is the optimal classifier batch size?

A: There is no batch-size setting; each `process_torrent` queue job carries whatever batch of info-hashes the crawler queued, and `CLASSIFIER_CONCURRENCY` (default `10`) is a semaphore on how many torrents classify in parallel. Higher values improve throughput but increase memory, Postgres connections and GC pressure. Monitor `go_gc_duration_seconds` and reduce if pauses exceed 200ms.

### Q: How does BEP-9 concurrency impact crawl speed?

A: Concurrency follows `DHT_CRAWLER_SCALING_FACTOR` (default `10`): the metainfo fetcher runs 40 × that many BEP-9 requests unless `DHT_CRAWLER_METAINFO_CONCURRENCY` overrides it. Raising it increases crawl speed at the cost of CPU, RAM and Postgres write load; the per-peer query limiter (`DHT_SERVER_QUERY_LIMITER_RATE_PER_SEC`, `DHT_SERVER_QUERY_LIMITER_BURST`) keeps any single peer from being flooded regardless.

### Q: What retention policy prevents disk bloat?

A: The `retention` worker, off by default. `RETENTION_ENABLED=true` is the dry run — it counts what it would delete (`bitagent_retention_would_purge_total`) and touches nothing; `RETENTION_ENABLE_PURGE=true` makes it real. A torrent is eligible when it is older than `RETENTION_MIN_AGE` (60d) and unseen for `RETENTION_MAX_LAST_SEEN` (180d). The `queueclean` worker (`QUEUECLEAN_ENABLED`, `QUEUECLEAN_ENABLE_PURGE`) does the same for finished queue jobs.

### Q: How do I tune Golang GOMAXPROCS?

A: Set `GOMAXPROCS=4` for 8-core hosts to avoid thread contention. Higher values yield diminishing returns. Verify with `pprof block` and `GODEBUG=gctrace=1`. Pin BitAgent to dedicated CPU cores via cgroups for latency-critical workloads.

## Customization

### Q: How do I customise the CEL classifier rules?

A: The rules are bundled in the binary and are not editable at runtime — changes ship as merge requests. `bitagent classifier show --format yaml` prints the live workflow (CEL rules plus content-type mapping) and `bitagent classifier schema` prints its JSON Schema; after a rule change deploys, `bitagent reprocess` re-classifies already-indexed torrents. See [concepts/classification.md](concepts/classification.md).

### Q: SQLite vs Postgres for evidence storage?

A: Postgres handles concurrent writes and large indexes efficiently for >50k entries. SQLite is acceptable for <20k caches but lacks connection pooling. Evidence always lives in Postgres; the only SQLite in BitAgent is the dashboard's own file at `/data/bitagent-ui.db` (settings overrides, audit log, hashed user keys).

## Project + community

### Q: What license governs BitAgent?

A: BitAgent is licensed under the MIT License. You may modify, distribute, and sublicense the code commercially without restriction. Source compliance requires retaining the LICENSE file and copyright notices.

### Q: Is BitAgent a fork of bitmagnet?

A: Yes. BitAgent is a 2026 fork of `bitmagnet-io/bitmagnet` after upstream went dormant in July 2025. We share the DHT protocol implementation and core indexing primitives but have rewritten classifier preempt, evidence pipeline, auth, observability, and release scaffolding.

### Q: How do I submit patches?

A: Fork `spencercnorton/bitagent`, create a feature branch, run `task lint test` locally, and open a PR with a clear commit message and updated docs. PRs require passing lint, unit tests, and Docker build checks before review.

### Q: What is the procedure for security disclosures?

A: Use GitHub's [private vulnerability reporting](https://github.com/spencercnorton/bitagent/security/advisories/new). Do not open public issues for vulnerabilities. We respond within 72 hours and provide CVE attribution upon patch.

### Q: How is governance structured?

A: A small core-maintainer group reviews proposals as merge requests; there is no separate roadmap file — open items live in the issue tracker and are prioritised by operator feedback and stability requirements. Major architectural shifts require consensus among core contributors.
