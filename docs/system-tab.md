# System Tab — Diagnostics & Tools

The System tab is the operator's debugging surface. Open it any time a metric on the Dashboard tab looks wrong, a Sonarr indexer test fails, or the dashboard responds with a non-200 you can't explain. Four sub-tabs: **Health Check**, **Search Test**, **GraphQL Explorer**, **Raw Metrics**.

## When to open the System tab

A short triage table:

| Symptom | First sub-tab to open |
| --- | --- |
| Dashboard stat cards stuck at 0 / `--` | Health Check → Connectivity Test |
| Sonarr/Radarr indexer test fails | Search Test |
| GraphQL queries hanging or 500ing | GraphQL Explorer |
| Grafana dashboards show wrong/missing series | Raw Metrics |
| `[Errno 111] Connection refused` in dashboard logs | Health Check (BitAgent Core card) |

## Health Check walkthrough

Health Check is three side-by-side cards, each verifying one subsystem.

**BitAgent Core card.** Live reachability derived from the last `/api/stats` poll:

- **GraphQL API** — `OK` if the core answered the GraphQL query behind `/api/stats`; `Unreachable` if it did not (core not running, URL wrong, core overloaded — open the core's container logs).
- **Torznab Endpoint** — **not probed.** The feed may share a server with GraphQL, but GraphQL success does not prove the Torznab route or its API-key path is healthy; use **Search Test** for that.
- **Metrics** — `OK` if the core's Prometheus `/metrics` endpoint returned data on the last poll. `OK` here is required for the Dashboard tab's stat cards to populate.

**Dashboard card.** The dashboard app itself:

- **App Version** — the `ui/version.py` version this page was rendered by.
- **SQLite DB** — `Connected` if a test `SELECT` on the sidecar database (`/data/bitagent-ui.db`) succeeded on the last poll. Common failure: the volume mount has the wrong permissions for the user the dashboard runs as.
- **Auth Mode** — which of the auth tiers are active (SSO, forwarded-user headers, NPM headers, API key), or `Open` when none is enforced (`REQUIRE_AUTH=false`).

**Network card.** The endpoints this dashboard is configured to talk to (from Settings, not from any socket):

- **Core GraphQL** — the configured `bitagent_graphql_url`.
- **Core Metrics** — the configured `bitagent_metrics_url`.
- **Dashboard** — this page's own origin.

The dashboard's listen port itself is `UI_LISTEN_ADDRESS` (see `configuration.md`); `--` on a row means the last poll has not completed yet.

## Connectivity Test

The **Run All Checks** button runs every Health Check probe synchronously and reports a per-check latency. Use it after:

- Firewall changes.
- VPN provider switches on the bitagent core (e.g. NordVPN → AirVPN).
- Container recreations where you've changed env vars.
- Adding a reverse proxy / SSO gateway in front of the dashboard.

Latency expectations: GraphQL and Metrics probes should be sub-50ms on localhost / private LAN, sub-200ms across continents. Over 1s on any check usually means a timeout retry rather than slow latency — check the core's load.

## Search Test recipes

Use the Search Test sub-tab when `*arr` says "0 results" or "test failed" and you want to know whether the corpus has the release at all. It takes a **Content Type** (All / Movie / TV Show / Music / eBook / Software) and a free-text **Query**, and runs them through the dashboard's own `/api/torrents` search — the same GraphQL-backed path the Library tab uses. It does **not** hit the `*arr`-facing `/torznab` endpoint, so a hit here with a miss in Sonarr points at the Torznab key, category mapping or the URL Sonarr was given; a miss here means the crawler has not seen the release yet.

For the real Torznab surface, query the core directly:

```bash
curl -s "http://localhost:3333/torznab?t=tvsearch&q=breaking+bad+s05e16&apikey=$TORZNAB_API_KEY"
curl -s "http://localhost:3333/torznab?t=movie&imdbid=tt0944947&apikey=$TORZNAB_API_KEY"
curl -s "http://localhost:3333/torznab?t=music&artist=Pink+Floyd&album=Animals&apikey=$TORZNAB_API_KEY"
```

Zero music results usually means the caps endpoint does not yet declare `music-search`: the core suppresses a category until it has classified at least one torrent in it, and Lidarr never sends the query until caps advertises it.

## GraphQL Explorer recipes

The Explorer is a minimalist text-area + Run button — not GraphiQL. No autocomplete, no schema browser. Use it to verify schema after a core upgrade.

### List the most recent 10 torrents

```graphql
query {
  torrentContent {
    search(input: { queryString: "", limit: 10 }) {
      items {
        infoHash
        title
        contentType
        seeders
      }
    }
  }
}
```

### Classifier recent decisions

```graphql
query {
  classifier {
    recentDecisions(limit: 10) {
      infoHash
      verdict
      score
      reason
    }
  }
}
```

(Schema availability depends on core version — check via introspection if the field is missing.)

## Raw Metrics

The Raw Metrics sub-tab streams `/metrics` in Prometheus exposition format. Use this to verify that the metric names your Grafana dashboards expect actually exist on this version of the core. Most useful counters:

- `bitagent_dht_peers_total` — current DHT routing table size. Should be in the hundreds within minutes of bootstrap, low thousands after a few hours.
- `bitagent_torrents_indexed_total` — cumulative count of admitted torrents. Monotonic; rate of change is the indexing throughput.
- `bitagent_classifier_decisions_total{verdict="..."}` — cumulative classifier decisions by verdict (`admit`, `reject`, `defer`). The reject:admit ratio is a useful spam pressure indicator.
- `bitagent_torznab_requests_total{category="..."}` — cumulative Torznab queries from `*arr`. If this is zero, your `*arr` isn't actually polling.
- `bitagent_csam_blocklist_export_total{outcome="..."}` — outcome of the post-classify CSAM defense hook (`blocked`, `skipped_no_match`).

For Grafana scraping, point the Prometheus job at the same URL the dashboard reads — `${BITAGENT_METRICS_URL}` — not at the dashboard. The dashboard is a consumer, not a producer.
