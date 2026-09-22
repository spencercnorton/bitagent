# The \*arr compatibility contract

BitAgent integrates with Sonarr, Radarr, Lidarr, Readarr and Prowlarr using
only their **stable, official interfaces**. It requires no forked or patched
\*arr build, no third-party plugin, and no interface that upstream has marked
deprecated. This page is the contract; a test pins it.

## What BitAgent uses

| Direction | Interface | Used for | Status upstream |
|---|---|---|---|
| \*arr → BitAgent | **Torznab** (`/torznab/api`, Newznab-derived) | Search and RSS; capabilities document; per-key profiles | Stable; the interface every indexer speaks |
| \*arr → BitAgent | **Connect → Webhook** notification, standard payload (`eventType`, `downloadId`, `downloadClient`, `release.releaseTitle`, `series`/`movie`) | Evidence. Accepted events: **`Grab`**, **`Download`**, **`DownloadFolderImported`**. Every other event type (`Test`, `HealthIssue`, `ApplicationUpdate`, `Rename`, deletes, …) is acknowledged with `200` and ignored | Stable |
| BitAgent → \*arr | **REST `/api/v3/history`** with the instance's API key | Optional backstop poller for lost webhook deliveries | Stable |
| BitAgent → qBittorrent | **WebUI API v2** (`/api/v2/auth/login`, `/api/v2/torrents/info`) | Optional category, tag and state poller | Stable |
| Prowlarr | Adds BitAgent as a generic Torznab indexer | Sync to the \*arrs | Stable |

Authentication is the \*arr's own API key outbound and a shared secret in the
`X-Evidence-Token` header inbound. There is nothing to install on the \*arr side.

## What BitAgent deliberately does not use

**A download-decision hook.** Some projects let an external service choose
*which* release an \*arr grabs by intercepting the decision. Sonarr and Radarr
have no such interface: the request is
[Sonarr issue #8396](https://github.com/Sonarr/Sonarr/issues/8396), open since
February 2026 with no assignee and no milestone, and the tools built on it ran
on patched \*arr forks that their own authors have since put on hold pending an
upstream implementation that has not arrived.

BitAgent does not need it. The one place an indexer can legitimately shape what
an \*arr grabs is the **Torznab response itself** — what is included, what is
excluded, and in what order — and BitAgent already owns that surface:

- **Ordering:** outcome priors learned from your own grab → import history
  ([`evidence.md`](../evidence.md)).
- **Exclusion:** dead swarms (liveness), tracker-confirmed zero-seeder releases
  and stale unknowns ([`swarm-health.md`](swarm-health.md)), verdict-ledger
  exclusions, and per-key profiles.
- **Honest unknowns:** a release with no tracker verdict is served without a
  `seeders` attribute so the \*arr's `minimumSeeders` check treats it as unknown
  rather than rejecting it at zero.

The \*arr's own quality profile and custom formats remain the final arbiter.
BitAgent changes what the \*arr sees and in what order; it never overrides what
the \*arr decides. If upstream ever ships a decision hook it can be adopted as an
addition — nothing here depends on it.

## How this is enforced

`internal/evidence/sources/arrwebhook/handler_test.go` asserts that the set of
accepted `eventType` values is exactly `{Grab, Download, DownloadFolderImported}`
for every supported application and that every other documented \*arr event is
ignored. Adding an event that only a fork emits fails the build. The poller
paths (`/api/v3/history`, `/api/v2/torrents/info`) are string constants in
`internal/evidence/sources/`.

## Version notes

- Sonarr v4 and Radarr v6 webhook payloads omit `applicationName`; BitAgent
  derives the source from the URL path segment first, then `instanceName`, so
  every current release is handled.
- `Download` and `DownloadFolderImported` are both treated as an import: older
  and newer \*arr versions differ in which they emit for a completed import.
