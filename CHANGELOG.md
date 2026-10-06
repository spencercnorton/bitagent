# Changelog

## Unreleased

- Apply deferred model receipts with source-bound compare-and-set transactions and retain committed facts across ordinary refreshes and response expiry.

- Add an opt-in durable request dispatch fence and bounded replay of retained
  first responses. Known pre-dispatch deferrals remain retryable; uncertain
  dispatches stay held even after ordinary capture retention.

- Add a disabled-by-default lifecycle for bounded optional model tasks, with
  source/policy identities, time-stratum capacity and owned worker leases.
  Provider dispatch fencing and targeted application are qualified separately
  before this worker can be enabled.
- Recognize explicit HEVC/H.265 and AV1 codec claims while preserving existing
  serialized codec values. Infer supported scene groups independently of codec
  recognition and retain unknown or contradictory metadata as unknown.

- Parse explicit `WEB-DL`, `WEB.DL` and `WEBDL` source tokens consistently,
  preferring compound source aliases before their shorter prefixes.
- Parse release attributes and resolve unique local identities after a new
  movie/TV type fallback, within the existing content policy. The continuation
  runs once and makes no additional model or metadata API request.
- Commit bounded quarantine expiry chunks together with their verdict state and
  audit events. Restore and explicit deletion commit their ledger transition
  atomically; failed chunks retain snapshots and remain retryable.

- Bind matcher extraction cache entries to rendered file evidence and request
  routing. Group responses have a separate exact-request cache and cannot warm
  single extraction. Backlog matching refuses grouped warming when evaluation
  capture is disabled; select a single-request chunk explicitly instead. Safe
  grouped workflow consumption requires a separate provenance-aware path.

- Preserve authentic torrent source metadata in quarantine snapshots and restore
  it without overwriting newer observations. Legacy snapshots remain usable.
- Record explicit restores as local provenance with unknown swarm counts, so
  the default Torznab freshness window can surface them without bypassing
  tracker-zero, liveness or verdict exclusions.
- Commit restore and rematch enqueue together. Queue failures retain the
  recovery snapshot; migration 55 refuses to discard retained source snapshots.

- Bound type fallback application with an audited category allowlist. A
  disallowed prediction remains unknown without triggering that category's
  deletion policy; changed policy invalidates cached application.
- Add an audited language review action that persists a model-attributed tag
  while retaining the torrent and ordinary deterministic filtering behavior.
- Document bounded application, actual-effect checks and independent rollback
  controls for optional model stages.

- Run optional type inference inside the classification workflow's exclusion
  policy. Deterministic outcomes remain authoritative; custom workflows without
  the policy boundary cannot apply a live prediction.
- Separate language-model enforcement from deterministic filtering with
  `CONTENT_FILTER_LLM_ENFORCE`, retaining legacy inheritance when unset. Skip
  model language inference for advertised English-inclusive releases and block
  model application when audit or model evidence is incomplete.
- Retain the actual grouped junk request, first response and position-bound
  decisions. Incomplete cycles cannot quarantine; manual deletion and blacklist
  updates commit atomically against an existing quarantine snapshot.
- Add an optional embedding shortlist before the chat matcher. It uses an
  explicit independent route, bounded vectors and the shared durable allowance.
  Full candidate identity checks and chat confidence still govern attachment.
- Reject contradictory, duplicate, incomplete and refused model responses in
  runtime and evaluation. Ambiguous batch IDs abstain instead of choosing a row.
- Add private offline blinded review packets and diagnostic scoring. These
  artifacts never grant production authority or replace independent gold,
  calibration and prospective acceptance. New live modes remain off by default.

- Make the public backend independently buildable and deployable without a UI.
  Remove the Python site and its Go worker, browser GraphQL playground and
  site-only documentation. Keep DHT processing, matching, classification,
  evidence, Torznab, GraphQL POST, metrics, CLI and all PostgreSQL migrations.
- Separate backend releases with a root `VERSION` file and Go-only container.
  Public examples and CI now validate the headless deployment boundary.
- Breaking packaging change for 4.0.0: the former `ui` worker, port 8080 and
  SQLite/site configuration are no longer shipped. Existing API deployments
  keep their schemas; operate browser applications as separate services.

## 3.5.0 — 2026-10-03

- Add private `music` and `generic` leaf releases to the offline publisher,
  protected catalogue, member library and private Torznab feed. Explicit
  `--include-music` Plex discovery includes track versions; Music and Other
  use Audio 3000 and Other 8000.
  Music supports text search only. Keep version-1 video manifests, torrent
  layouts, private key permissions and fresh seed verification unchanged.

- Align administration with discovery's shared teal and navy palette, lime
  product mark, typography, navigation and card treatments in both themes.
- Support an HTTPS library-origin `/admin` proxy with an explicit, fixed
  `OPERATOR_INGRESS_HOST`. Prefix operator API requests and navigation while
  retaining disjoint host allowlists, verified operator roles and owner CSRF
  checks. The proxy must strip `/admin` and set its fixed operator Host.
- Add optional `INVITATION_BRIDGE_INGRESS_HOST` for canonical-origin machine
  enrollment behind a fixed operator transport Host. The empty default keeps
  legacy exact-host behavior; startup seals, proof and origin signatures remain
  required, and the feature remains disabled by default.

- Show a freshly generated invitation code alongside its link, with a
  separate copy action and a clear account-menu entry for code generation.
  Clear both secrets together when hidden, refreshed or leaving the page.

- Add an explicit, disabled-by-default owner action to admit their own
  invitation issuer before private indexing. Bind it to the verified account
  on the canonical operator origin; retain suspended memberships and all
  existing account, key and private permissions.

## 3.3.0 — 2026-10-01

- Add explicit `openai`/`ollama` chat backend selection for the TMDB matcher
  and type fallback. Keep `openai` as the default; Ollama requests use
  `max_tokens` and `reasoning_effort: "none"` with the existing requested caps.
  Reject incompatible provider and endpoint settings without changing stage
  enablement, live gates, budgets or privacy checks. Backend compatibility does
  not establish model accuracy or calibrated confidence.

## 3.2.1 — 2026-10-01

- Add manual invite-code entry and an explicit, disabled-by-default site
  registration mode so profile enrollment can run before private indexing.
  Preserve signed enrollment, issuer membership and suspension checks, while
  private routes and payment checkout remain closed in public-only mode.
- Partition site-registration preview limits by a proven proxy's canonical
  original connection address. Invalid or untrusted headers retain the
  transport-peer limit and never supply identity or membership authority.

## 3.2.0 — 2026-10-01

- Apply startup vault settings as one validated batch. A rejected payload keeps
  the original configuration intact, including operator roles and credentials.
- Connect the user library and operator console with an admin-only Admin
  navigation entry and a Back to library link. Destinations use validated,
  startup-only configuration or the existing host allowlists; authorization
  is still checked independently on the destination surface.
- Organize Settings into four workflow groups with typed logging, URL and
  secret controls, grouped deployment references and reduced-motion-aware
  section transitions.
- Align AI observations with the core's token labels and type-classifier
  metrics. Preserve missing counters as unknown, expose source observations,
  and label cost totals and sampled projections as estimates rather than
  invoices or model accuracy.
- Refine the operator console with bookmarkable navigation, keyboard-safe mobile
  navigation and per-field draft preservation.
  Runtime overrides expose save/reset progress and failures, with an explicit
  reset step and no browser persistence of replacement secrets.
- Show loading, stale, measuring, unavailable and failed operational observations
  alongside their values. Suppress live indicators on stale snapshots, distinguish
  aggregate response time from source observation time, and leave gaps in grab
  trends where no ratio was measured.

## 3.1.0 — 2026-10-01

- Automatically open profile enrollment for every invitation recipient when the optional
  enrollment bridge is enabled, including existing signed-in users. Require
  that path on the backend and support a signed site-local account provider
  alongside SSO; profile and password handling remain in the identity service.

## 3.0.0 — 2026-09-30

- **Breaking:** All existing and new generic personal indexer keys default to
  public-only access. Existing private-client integrations must explicitly
  rotate to a private-enabled key through verified human membership, then
  update their indexer connections and personalized torrent credentials.
- Private responses and tracker peers recheck the key capability, ownership,
  revocation and membership. Public DHT integration keys remain independent
  of private membership.
- Human-member private catalogue browsing retains its membership policy;
  the additional key permission controls personal-key private Torznab and
  personalized torrent/tracker credential issuance.

## 2.15.0 — 2026-09-30

- Add an optional explicit SSO enrollment subject format while preserving
  legacy account IDs by default. Bind enrollments to their startup format,
  reject cross-format membership collisions and require a separate reviewed
  account-data migration before enabling canonical principal subjects.

## 2.14.0 — 2026-09-30

- Add optional single-use member invitations with a permanent UTC-year
  allowance, exact-subject owner limits, revocation and atomic SSO redemption.
  Preserve existing account IDs; invitations do not enroll network peers or
  establish content readiness. Paid-credit receipts are separate and idempotent.
- Add a separate optional, purpose-signed SSO enrollment bridge with bounded
  replay state and exact principal binding. Preserve denial history and keep
  provider approval, project-capability activation and network admission under
  their own verified boundaries.
- Add disabled-by-default hosted Stripe Checkout for one fixed invitation
  credit, with durable orders, signed event reconciliation and terminal
  refund/dispute withdrawal. Browser returns grant no credit; uncertain
  outcomes retain their original payment key and require reconciliation.

## 2.13.1 — 2026-09-30

- Refresh private-seed availability in bounded publication-order batches with
  short SQLite transactions and original observation timestamps. Withdraw the
  complete proof epoch on failures, cancellation or deadline expiry; recovery
  cannot reactivate old proofs or an in-flight withdrawn/re-published release.
- Enforce one private-readiness application owner on a protected local POSIX
  database, while keeping disabled-default imports portable. Recheck exact
  proofs before private results, torrent/magnet issuance and tracker writes.
- Index expired private peers so tracker cleanup does not scan the full ledger.

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
