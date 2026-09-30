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

## Public library

The library is a separate discovery surface selected by `PUBLIC_LIBRARY_HOSTS`.
It uses the existing authentication boundary; internet accessibility does not
enable anonymous access or operator controls.

Use Discover, Movies, and Series to browse, or focus search with `/` or
`Ctrl/Cmd+K`. Combine genre, year range, quality, source, language, and technical
features in Filters. On smaller screens the Filters button opens these controls.
Active filters appear above results; remove one chip or choose Clear all.
The URL preserves search, filters, sorting, and the current page for sharing.

The spotlight rotates artwork from popular and newly released movies and shows.
Pause it or select a title with the arrow/dot controls. Keyboard focus, pointer
hover, off-screen/hidden pages, and reduced-motion preferences pause rotation.
**Find releases** checks the exact title identity against the index.

Choose a streaming service under **Browse by service**, or search for its name.
Select your country, then use the main search and Movies/Series tabs to navigate
that provider's catalogue. These titles come from TMDB with JustWatch provider
availability; open a title to check for indexed releases. Provider catalogues
include subscription, free, ad-supported, rental, and purchase availability.
Provider-specific text search checks each candidate's regional availability;
empty filtered pages can still have a next page, and failed checks are disclosed
as partial results. Provider/country/search/page state is shareable in the URL.
Artwork and provider discovery require the operator's own `TMDB_API_KEY`; if
metadata is unavailable, normal indexed-library search continues to work.

Open a title to inspect releases and seasons. Every release has a checkbox,
**Copy** magnet action, and **Open** link for the system's torrent client.
**Copy title magnets**, **Copy show magnets**, and **Copy season magnets** copy
links directly using the current quality/source filters. Recommended
selection favors packs and seed counts; **All matching versions** retains
alternate releases. Filename parsing cannot establish that a pack is complete,
and overlapping packs may include duplicate episodes. Review the selected
release names before copying them. A direct copy is bounded to 1,000 links;
partial selections are disclosed.

Release checkboxes and the **Magnets** button support a custom collection, including links selected from
different titles. Remove individual releases, copy the newline-separated
magnet list, save a `.txt` file, or expand View magnet links for manual copying.
Each line identifies one torrent; a list of links is not a new combined torrent.
The collection stays in the current tab and clears on refresh.

Title detail scans at most 12 pages of 250 raw search rows (3,000 rows).
An empty filtered page does not end pagination. When further rows remain, the
detail page explicitly reports a partial selection; recommendations describe
available indexed releases and do not guarantee a whole season or show.
Closing or switching the title cancels the pending scan. Metadata failure does
not prevent using loaded releases. Motion respects the browser preference,
and dialogs contain keyboard focus and restore it on close.

The account sidebar shows all-time aggregate activity since tracking began.
**Grabs** counts magnet links successfully copied or passed to open/export
actions; repeated actions count again. **API searches** counts successful
authenticated Torznab search feeds. These counts survive API-key rotation and
store no release names, hashes, URLs, or keys. Downloaded/uploaded bytes,
ratio, and hit-and-run totals require client or tracker reporting, so unavailable
values appear as **Not reported** rather than inferred transfers or zeroes.
