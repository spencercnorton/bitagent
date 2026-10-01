# Changelog

## Unreleased

## 2.13.0 — 2026-09-30

- Include all UI runtime modules, templates and static assets in installed wheels.
- Add separate, searchable streaming-service and TV/cable network browsers with
  logo tiles, accessible selection controls, and paged network discovery.
- Recover streaming logos across movie and series catalogues, try safe alternate
  images, and retain named tiles when an upstream logo is unavailable.
- Group subscription plans and storefront channels into one service option;
  search their combined regional catalogue and retain variant-name searches
  and existing provider links.
- Filter network series and title searches by verified TMDB network identities;
  preserve network selections in shareable URLs and browser history.
- Bound complete metadata requests and network export downloads, including
  queued work, to keep upstream failures responsive.
- Refine library navigation, typography, spacing and responsive discovery;
  show a compact service roster with full-catalogue search and disclosure.
- Add clear catalogue headings and loading, empty and unavailable states;
  retain selected brands and keyboard focus during discovery changes.
- Keep title identity visible when artwork fails and bring mobile release
  actions forward with expandable quality, source and edition filters.
- Preserve the full name and separate identity of The Roku Channel.
- Add account-scoped display, search and magnet preferences with clear save,
  browser-storage and retry states; preserve explicit shared-link filters.
- Show copied/opened/exported activity, tracked dates, observation time and
  seven/thirty-day UTC windows with explicit coverage notices.
- Deduplicate retried activity receipts and isolate delayed account responses;
  prevent anonymous/shared credentials from becoming personal usage totals.
- Add bounded operator catalogue pages and optional import acknowledgments
  that reconcile every source alias with its canonical private torrent.
- Keep catalogue scans consistent across readiness refreshes, metadata edits,
  process restarts and restored database snapshots.
- Add optional short-lived device address ownership for private tracker
  credentials, with authenticated seed snapshots, expiry and replay checks,
  and current-member filtering of advertised peers and swarm counts.
- Keep private tracker byte counters monotonic within active peer sessions and
  return full client-reported account totals beyond the release display limit.

## 2.12.1 — 2026-09-30

- Recognize qBittorrent 5.2 login responses and session cookies when checking
  private seeder readiness. Keep failed authentication, unusable cookies and
  incomplete seed copies unavailable.

## 2.12.0 — 2026-09-30

- Add an optional private media catalog alongside the public DHT catalog, with
  grants for existing SSO identities, revocable member tracker credentials,
  personalized private torrents and separate client-reported transfer metrics.
- Prepare and verify real movie, episode, season and show torrents from a
  read-only Plex inventory or explicit file manifest; advertise only freshly
  verified seed copies and retain separate metadata aliases for shared swarms.
- Add a private library and membership administration page, private Torznab
  feed, and candidate Jackett definitions for both catalog sources.
- Keep credential-bearing request URLs out of application access logs.
- Stream large Plex inventories into bounded catalog shards with resumable hashing,
  atomic publication generations, and independent full-file verification.
- Add optional shared read-rate limits for publication and verification hashing,
  without throttling metadata, file-stat checks, or unchanged cache reuse.
- Fail closed on interrupted inventory pagination, unavailable seed copies, and
  stale readiness after process restart; require verified seed registration before
  advertising private releases.

### Security

- Update pgx, CEL, mapstructure, Ed25519, compression and Go crypto/network/Unicode
  dependencies to their fixed releases, with required transitive module updates.
  Use the supported Go 1.26.8 toolchain in development, CI and the pinned container
  build; check called Go dependencies for known vulnerabilities in public CI.

## 2.11.0 — 2026-09-30

- Redesign the public library with colorful discovery navigation, animated cards,
  responsive filters, light/dark themes, and reduced-motion support.
- Surface advanced search as removable filters and preserve shareable browse URLs.
- Replace the introductory card with a rotating popular/new movie and series
  artwork banner, with pause controls and direct indexed-release lookup.
- Add cached streaming-provider catalogues, country selection, provider search,
  and provider-specific movie/series search with explicit availability checks.
- Add individual magnet copy/open actions and a reviewable collection for titles,
  seasons, shows, and manually selected releases, with copy and text export.
- Replace the collection prompt with direct title/show/season magnet copy controls.
- Persist private per-account aggregate grab and successful Torznab-search totals;
  show transfer and hit-and-run metrics as unreported until client reporting exists.
- Avoid retaining Torznab query-string credentials in raw UI access/error logs.
- Page title releases within explicit bounds and disclose partial results.
- Preserve collection history, dialog focus, and title identity during async metadata loads.
- Verify Linux amd64 release images against their source revision and UI version;
  exclude local credentials, environments, and state from Docker builds.

## 2.10.9 — 2026-09-27

- Establish GitHub pull requests as the development workflow, with privacy checks.
- Add deployment, configuration, security, upgrade and recovery documentation.
- Remove the inherited shared metadata key and automatic fallback; enrichment
  requires the operator's own TMDB_API_KEY.

- Fail closed when retained content-filter evidence loses its privacy, expiry
  or source-identity admission, with PostgreSQL regression coverage.
