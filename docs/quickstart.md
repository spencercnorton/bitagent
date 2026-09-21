# Quickstart: 15 Minutes to Indexing

Follow this exact sequence to deploy BitAgent, seed the DHT network, and connect it to Sonarr. You will have a fully functional Torznab indexer running on `localhost` by the end of this guide.

## 1. Prerequisites

Ensure your host meets the baseline resource and network requirements before proceeding. BitAgent requires Postgres 14 or later; if you lack an external instance, the bundled database service in the example compose file is fully supported and requires zero configuration. Docker and Docker Compose must be installed and accessible to your user. Verify with `docker info` and `docker compose version` before continuing.

Allocate a minimum of **2 GB RAM** and **10 GB persistent disk** for the first month of operation. BitAgent caches magnet metadata, DHT node tables, and query logs during the warmup phase. Insufficient allocation will trigger OOM kills or disk write stalls.

Network egress must allow unrestricted **outbound TCP and UDP traffic**. The DHT protocol relies on UDP `4413` (default) and TCP `80/443` for tracker fallback. Inbound UDP must reach the BitAgent container; behind NAT, forward UDP `4413` to your container host. Closed UDP will prevent node discovery and cripple search response times.

## 2. Get the source

The public quickstart builds the image from this repository. Clone it:

```bash
git clone https://github.com/spencercnorton/bitagent.git
cd bitagent
```

`examples/docker-compose.public.yml` orchestrates two services: `bitagent` (the Go indexer plus its `ui` worker, built from the checkout) and `postgres:16`. Port `3333` serves GraphQL, Torznab, Prometheus metrics and the *arr evidence webhooks; port `8080`, bound to loopback, serves the operator dashboard with `REQUIRE_AUTH=false`. There is no bundled reverse proxy or auth layer — put one in front before exposing either port beyond your LAN (see [examples/README.md](../examples/README.md) and [ui/README.md](../ui/README.md) → Authentication).

## 3. Configure secrets

Copy the template and generate the two values that matter:

```bash
cp examples/.env.example examples/.env.public
openssl rand -hex 32   # this is your POSTGRES_PASSWORD
openssl rand -hex 32   # this is your TORZNAB_API_KEY
```

Edit `examples/.env.public`:

```env
POSTGRES_PASSWORD=<paste_first_generated_value>
TORZNAB_API_KEY=<paste_second_generated_value>
```

`POSTGRES_PASSWORD` is required; Compose refuses to start while it is blank. `TORZNAB_API_KEY` is optional, but leaving it empty serves `/torznab` unauthenticated — fine on a private network, not on the open internet. Never commit `examples/.env.public`.

## 4. First start

```bash
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build
```

The first `up` builds the image from source — expect 2–4 minutes on a warm Docker. Then wait **3 minutes** before querying: the DHT bootstrap needs time to locate peers and build its routing table, and queries before that return empty results.

Verify both services are healthy:

```bash
docker compose -f examples/docker-compose.public.yml ps
```

You should see `bitagent` and `postgres` in `Up (healthy)`. If either shows `Restarting` or `unhealthy`, run `docker compose -f examples/docker-compose.public.yml logs <service>`. Common causes: a port conflict on `3333`, insufficient disk space, or firewall rules blocking UDP egress.

## 5. Sanity check

Validate the endpoints before connecting Sonarr:

```bash
# the crawler is persisting torrents
curl -s http://localhost:3333/metrics | grep bitagent_dht_crawler_persisted_total

# Torznab capabilities
curl -s "http://localhost:3333/torznab?t=caps&apikey=$TORZNAB_API_KEY" | head -20
```

Expected: a `bitagent_dht_crawler_persisted_total{entity="torrent"}` counter that climbs over the first minutes, and valid Torznab `<caps>` XML. A 401 on `/torznab` means the key does not match `examples/.env.public`. The GraphQL playground is at `http://localhost:3333/graphql`.

Then open the operator console at `http://localhost:8080` — the Dashboard tab's **Crawl Throughput** card (the classifier's examine rate) and **Indexed Torrents** start moving within a few minutes, and **System → Health Check** confirms the core's GraphQL and metrics endpoints are reachable. The public library is the same port under `http://library.localhost:8080`. Both are loopback-only with no login in this stack; `UI_ENABLED=false` in `.env.public` turns the dashboard off entirely.

## 6. Add to Sonarr

Open Sonarr → `Settings → Indexers → Add → Torznab → Custom`:

- **Name:** `BitAgent`
- **Enable:** ✓
- **URL:** `http://<docker-host>:3333/torznab` *(use the host's LAN address; `localhost` will not resolve from inside Sonarr's container, and the compose service name `bitagent` only resolves from containers on the same compose network)*
- **API Key:** paste your `TORZNAB_API_KEY`
- **Categories:** select TV, Movies, or all per your library
- **Priority:** `25` (or whatever fits your stack — leave the default if unsure)

Click **Test**. The button turns green with `Test was successful`. If you see `Connection failed`, Sonarr cannot reach port `3333` at the address you entered. Save the indexer.

## 7. Watch first grabs

Trigger a manual search in Sonarr (a single episode is enough). Within **5–10 minutes** the persisted-torrent counter on `/metrics` keeps climbing and searches return results as Sonarr's first poll cycle completes.

If nothing appears after 10 minutes, check:
- `bitagent_dht_crawler_persisted_total` is increasing
- Sonarr's indexer test came back green
- the container logs for a DHT bind or port conflict

## 8. What's next

BitAgent is now operational and feeding your *arr stack. Continue with:

- [Configuration](configuration.md) — every env var, every default
- [Dashboard guide](ui-guide.md) — the console's tabs, and [ui/README.md](../ui/README.md) for its auth tiers before you expose it
- [Troubleshooting](troubleshooting.md) — port mapping, DHT starvation, logs
- [Classification](concepts/classification.md) — the classifier pipeline and CEL rules
- [Monitoring](operations/monitoring.md) — Prometheus scrape job and the bundled Grafana dashboard

For private-tracker-style setups (Prowlarr direct, api-key only, no SSO), see [docs/integrations/private-tracker-mode.md](integrations/private-tracker-mode.md).
