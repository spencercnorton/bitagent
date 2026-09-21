# Troubleshooting

Symptoms, diagnoses, and fixes for the most common issues. Each entry: the **one** check to run, the **one** change that resolves it.

## Dashboard shows "Connect to BitAgent core to see category data"

### Symptom

The Category Breakdown panel on the Dashboard tab is stuck on the placeholder text. Stat cards may also show `0` or `--`.

### Diagnosis

From inside the container (the dashboard is the `ui` worker in the same container as the core), probe the core's GraphQL endpoint:

```bash
docker exec bitagent curl -sf "${BITAGENT_GRAPHQL_URL:-http://127.0.0.1:3333/graphql}" \
  -X POST -H "Content-Type: application/json" \
  -d '{"query":"{ __typename }"}'
```

If this returns a connection refused, the core isn't running or the URL is wrong. If it returns a GraphQL envelope, the core is fine — refresh the dashboard.

### Fix

Verify `BITAGENT_GRAPHQL_URL`:

- Same Docker network: `http://bitagent:3333/graphql`
- Host network mode (host-network): `http://127.0.0.1:3333/graphql`
- Compose-bridged: use the service name, not `localhost`.

Restart the dashboard container after fixing.

## DHT PEERS = 0 after 10+ minutes

### Symptom

The DHT PEERS stat card stays at `0` (or single digits) more than ten minutes after bringing the core up.

### Diagnosis

UDP/4413 inbound is closed. The crawler made outbound `find_node` calls but no peer can respond — the routing table never fills.

```bash
# From a different host on the public internet, send a probe:
nc -uvw5 <your-public-ip> 4413 < /dev/null
```

If `nc` reports timeout, the port is closed.

### Fix

Open UDP/4413 inbound on your firewall and forward to the bitagent core container. NAT-PMP/UPnP works if your router supports it. Reachable inbound is the single biggest factor in DHT yield — closed ports cap you at ~20% of full crawl rate.

## EVIDENCE EVENTS stays at 0 after enabling `*arr` webhooks

### Symptom

You configured Sonarr / Radarr webhooks pointing at `/api/evidence` and clicked Test in `*arr` (it said success), but the dashboard's EVIDENCE EVENTS counter and Evidence tab stay empty.

### Diagnosis

In the `*arr` Settings → Connect → Webhook → click **Test**. Inspect the response in the `*arr`'s system log:

- `200 OK` → dashboard accepted it. Refresh the Evidence tab.
- `404 Not Found` → URL is wrong (commonly missing `/api/`).
- `401 Unauthorized` → `REQUIRE_AUTH=true` but the URL doesn't carry the API key.
- Network error → the `*arr` can't reach the dashboard at all (DNS, firewall, wrong host).

### Fix

The webhook URL must be the full path: `https://bitagent.example.com/api/evidence` — not `/evidence`, not the dashboard root. If `REQUIRE_AUTH=true`, append `?apikey=$DASHBOARD_API_KEY` to the URL or set `TRUST_NPM_HEADERS=true` and let your reverse proxy inject the user header.

## Library tab "No torrents found" but DHT PEERS > 0

### Symptom

DHT is alive (peers in the hundreds or thousands) but the Library shows `No torrents found`.

### Diagnosis

Three possibilities, in order:

1. The search box has stale text from a previous session — clear it.
2. The "All types" filter is set to a category the core hasn't classified into yet — reset to `All types`.
3. The classifier hasn't admitted anything yet (first ~30 min after boot, the classifier vets candidates conservatively).

```bash
docker compose logs bitagent | grep -i "classifier" | tail -20
```

You should see `verdict=admit` lines after the first half hour.

### Fix

Wait. The classifier is intentionally conservative on cold boot to avoid admitting the first wave of DHT noise. If after an hour the Library is still empty and the logs show only `verdict=reject`, your classifier is too strict — lower the admission threshold in **Settings → Classifier**.

## "401 Unauthorized" on every dashboard endpoint

### Symptom

Every dashboard request returns `401`. The page itself may load (`/healthz` is unauthenticated) but every `/api/*` call fails.

### Diagnosis

```bash
curl -sf "https://bitagent.example.com/api/me?apikey=${DASHBOARD_API_KEY}"
```

If this returns `200` with the identity JSON, the key works — the browser isn't passing it. If it returns `401`, the key itself is wrong.

### Fix

- **For browser sessions**: set up auth via reverse proxy. Either NPM (`TRUST_NPM_HEADERS=true`) or Authelia/oauth2-proxy/Cloudflare Access (`TRUST_FORWARDED_USER=true`). The browser doesn't carry an API key — it carries a header from the proxy.
- **For scripted access**: pass the key as `?apikey=`, `Authorization: Bearer <key>`, or `X-API-Key: <key>`.

Confirm the active auth mode on **System → Health Check → Dashboard card → Auth Mode**.

## Sonarr/Radarr Torznab indexer "Test failed: 401"

### Symptom

Sonarr/Radarr Torznab indexer test fails with `401 Unauthorized`.

### Diagnosis

The `*arr`'s Torznab API Key field is not the same value as the bitagent core's `TORZNAB_API_KEY`. **It is also not the same as `DASHBOARD_API_KEY`** — the two keys are independent.

### Fix

Copy `TORZNAB_API_KEY` from the core's env (or generate a new one with `openssl rand -hex 32`, set both, and restart the core). Paste it into Sonarr/Radarr → Settings → Indexers → BitAgent → API Key. Test again.

Never reuse `DASHBOARD_API_KEY` for `TORZNAB_API_KEY` — they have different threat models. The Torznab key is used by automated `*arr` agents from anywhere on your tailnet; the dashboard key gates an interactive operator UI.

## Library posters all show text fallback

### Symptom

Library grid shows text-based placeholders instead of TMDB poster art.

### Diagnosis

```bash
docker compose logs bitagent | grep -i tmdb | tail -10   # the ui worker's lines carry the logger name `ui`
```

You'll see one of: `TMDB_API_KEY unset`, `tmdb 401 invalid api key`, `tmdb 429 rate limited`.

### Fix

- Unset → register at <https://www.themoviedb.org/settings/api> (free), then set `TMDB_API_KEY` in the dashboard env, restart.
- Invalid → re-check the key character-for-character. TMDB v3 keys are 32 hex chars.
- Rate limited → wait. The dashboard caches successful poster fetches in SQLite; rate-limit dies as the cache fills.

## "Database is locked" errors in dashboard logs

### Symptom

Dashboard logs show `sqlite3.OperationalError: database is locked`. UI may briefly stutter on writes.

### Diagnosis

Two processes writing the same SQLite file. The core runs exactly one uvicorn process for the `ui` worker; the usual cause is a second copy — a standalone `uvicorn app:app` from a `ui/` checkout, or another container — pointed at the same `/data` volume.

### Fix

Stop the second writer. The dashboard is single-tenant and small enough that one process handles peak load fine; never add `--workers` to the spawned command.

## Container healthcheck flapping

### Symptom

`docker ps` shows the bitagent container alternating between `(healthy)` and `(unhealthy)`. The dashboard works in the browser.

### Diagnosis

```bash
docker inspect bitagent --format '{{json .State.Health}}' | jq
```

Look at the most recent `Output` field. The quickstart healthcheck probes the core's `/metrics` on `127.0.0.1:3333` and, when `UI_ENABLED=true`, the dashboard's `/healthz` on `127.0.0.1:8080`. A `Connection refused` on 8080 means the `ui` worker is not listening: either it is restarting (its log lines carry the logger name `ui` in `docker compose logs bitagent`; startup validation failures such as overlapping `OPERATOR_HOSTS` / `PUBLIC_LIBRARY_HOSTS` exit with code 3 and are retried with backoff) or the in-container port was changed without changing the probe.

### Fix

Fix what the `ui` log lines say, or align the healthcheck with the port you configured. If you run the crawler headless, keep `UI_ENABLED` unset so the probe skips the dashboard.

## High Postgres CPU after first 24h

### Symptom

The bitagent-postgres container's CPU is pegged at 60-100% sustained, 24+ hours after first boot. Disk I/O is also high.

### Diagnosis

Autovacuum hasn't caught up with the DHT crawl ingest rate. The core writes a lot of small rows into `torrent_files`, `torrent_contents`, `queue_jobs`; default autovacuum thresholds are too lazy.

### Fix

Tune Postgres in the compose service env:

```yaml
command:
  - postgres
  - -c
  - autovacuum_vacuum_scale_factor=0.05
  - -c
  - autovacuum_analyze_scale_factor=0.02
  - -c
  - autovacuum_naptime=10s
```

CPU should drop within an hour as autovacuum catches up. Persistent high CPU after that means your write rate exceeds your IOPS budget — move the volume to faster storage.
