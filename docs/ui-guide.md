# Dashboard UI Guide

## Overview

The dashboard is the `ui` worker inside the BitAgent image: a small FastAPI app under [`ui/`](../ui/README.md) that the Go core starts when `UI_ENABLED=true` (or alone with `UI_ENABLED=true bitagent worker run --keys ui`), supervises, and restarts with backoff. It reads the core over GraphQL and Prometheus and never writes to the corpus; its own state — settings overrides, block phrases, notifications, hashed user API keys — lives in one SQLite file at `/data/bitagent-ui.db`.

One app serves two hostnames. `OPERATOR_HOSTS` selects the **operator console** described on this page; `PUBLIC_LIBRARY_HOSTS` selects the **public library** (poster grid, search and facets, title pages, and an account panel where each user mints a personal Torznab key). Any other `Host` is answered `421`. The quickstart binds the console to `http://localhost:8080` and the library to `http://library.localhost:8080`.

Use the console for: confirming the crawler is healthy, seeing what your \*arrs are still missing, verifying that \*arr webhooks reach the evidence pipeline, spot-checking the junk-purge quarantine, watching what each LLM stage costs, and troubleshooting connectivity from the System tab. Everything you can do here you can also script with `curl` against the same `/api/*` endpoints.

## Layout

A left **sidebar**, a top **header bar**, and a scrollable **main content area**. The sidebar has three groups:

- **Overview** — Dashboard, Library
- **Operations** — Wants, Evidence, Quarantine, AI
- **System** — Settings, System

The sidebar footer carries a **Dark mode** toggle (persisted to `localStorage` under `bitagent-theme`) and an avatar showing the first letter of the authenticated identity's display name. The header has a **Notifications** bell and a **Refresh** button that re-fetches the visible tab. Every stat card, section header and table column carries a small ⓘ with a plain-English explanation of what it measures and where the number comes from.

## Dashboard tab

The at-a-glance health view. The north-star card is **Indexer Win Rate (30d)** — the share of \*arr grabs won by BitAgent against every other indexer, from the core's grab evidence. Beside it: **Match Rate**, **Grab Liveness** (whether the swarm was alive when grabbed, not whether the grab succeeded), **Crawl Throughput**, **Indexed Torrents** and **Dead Blocked**. Every headline is source-aware: an unavailable value renders as *unknown*, never as zero, and a rate whose baseline is still filling says `measuring…`.

Below the cards: a category breakdown of the corpus, uptime, and the recent-activity feed.

## Library tab

A sortable, searchable table of recently crawled torrents with posters from TMDB when `TMDB_API_KEY` is set. A row opens a detail panel with the magnet URI to copy and, when `SONARR_*` / `RADARR_*` / `LIDARR_*` are configured, a send-to-\*arr button.

## Wants tab

The crawler's *wantbridge*, reported as observations: **Wanted Titles** (what Sonarr/Radarr/Lidarr are still missing), **Exact Matches** found in the corpus, **Fingerprint Keys** and **Poll Cycles**. Wants are not created here — they come from the \*arrs the core polls; see [Wantbridge](concepts/wantbridge.md).

## Evidence tab

Per-source **Source Health** first — received and persisted counters per webhook source — then the raw evidence stream. See [Evidence Pipeline](evidence.md) for the \*arr webhook setup.

## Quarantine tab

The junk-purge hold: every row shows name, confidence, when it was quarantined and days left. **Restore** re-inserts the row and re-queues a rematch, **Delete now** drops it, and **Spot-check 25** pulls a random offset — the honest way to sample a queue nobody will read through. Bulk restore works on the selected rows.

## AI tab

One scorecard per LLM stage the crawler runs (TMDB matcher, content filter, junk judge) on shared axes: model, throughput, latency, a quality *proxy* (there is no ground truth, so nothing is called accuracy) and spend. The header row shows **Projected / month** against `LLM_MONTHLY_BUDGET_USD`, **Tokens**, **Spent This Boot** and **Models Billing**. Money is never guessed: an unpriced model makes the total a floor (`≥ $x`), and an idle stage is *still measuring*, not free.

## Settings tab

Read-only: the configuration the running container sees, split into **Configuration**, **Auth & Security**, **Integrations**, **Retention**, **Classifier**, **Liveness**, **Filters**, **Block Lists** and **Audit Log**. Secrets render as `🔒 •••••• <n> chars`. The only values that can be overridden from here are the integration credentials in `MUTABLE_FIELDS` (`ui/config.py`: TMDB, Torznab and the \*arr URLs and keys); they are never echoed back and their audit rows store `[redacted]`. Host lists, proxy trust, proxy CIDRs, the proof secret, operator roles and the upstream URLs are startup-only.

## System tab

The diagnostics surface. Four sub-tabs:

- **Health Check** — per-endpoint reachability cards and a **Connectivity Test** panel whose **Run All Checks** button fetches `/healthz`, `/api/me`, `/api/stats` and `/api/metrics` and reports OK/FAIL, status and round-trip time.
- **Search Test** — runs a search against the core's indexed catalogue through the dashboard's own `/api/torrents` query (the path the Library uses); it does not hit the \*arr-facing Torznab endpoint.
- **GraphQL Explorer** — a text area and a Run button, no autocomplete. Use it to confirm the schema after a core upgrade.
- **Raw Metrics** — the core's `/metrics` in Prometheus exposition format.

See [System Tab — Diagnostics & Tools](system-tab.md) for recipes.

## Authentication

The app validates no passwords and no session cookies. With `REQUIRE_AUTH=false` — the quickstart's setting — every endpoint is open, which is why the quickstart binds port 8080 to loopback; **never** expose it that way. With `REQUIRE_AUTH=true` (the default) identity is resolved through three tiers, in order:

1. **`DASHBOARD_API_KEY`** — `?apikey=`, `X-Api-Key` or `Authorization: Bearer`. A wrong key is `401` with no fall-through.
2. **`X-Auth-User-Id`** from a reverse proxy, when `TRUST_NPM_HEADERS=true`.
3. **`X-Forwarded-User`** from a reverse proxy, when `TRUST_FORWARDED_USER=true`.

The proxy tiers are honoured only when the transport peer is inside `TRUSTED_PROXY_CIDRS` **and** the request carries `X-BitAgent-Proxy-Proof` equal to `PROXY_AUTH_SECRET`; enabling a tier without both fails startup. Hostname selects a surface, it never grants authority: only a verified `X-Auth-Priv` value in `OPERATOR_ROLES` reaches the operator console, and operator APIs are `404` on the public host. The full table of settings is in [`ui/README.md`](../ui/README.md).

## Theming and accessibility

The **Dark mode** toggle flips the theme instantly and persists it in `localStorage`; the browser's `theme-color` follows. `prefers-reduced-motion` is honoured.
