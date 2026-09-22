# Evidence pipeline

Evidence is what makes BitAgent more than a Torznab front-end on a DHT crawl.
Every time one of your \*arrs grabs a release from BitAgent and every time it
imports one, that fact comes back as a webhook and is recorded. From those
records BitAgent derives four things: an authoritative label for the torrent
that short-circuits classification; the right identity for *other* torrents
of the same title, so same-name shows stop being confused; a per-feature
success prior that re-ranks future search results toward what has actually
worked for you; and a liveness state that keeps dead swarms out of your
\*arr's view.

Nothing here is learned by a model. The loop is explicit, inspectable, and
every step has a metric.

## The loop

```text
DHT crawl → classifier → Torznab → *arr grabs → download client → *arr imports
                                       │                              │
                                       └── Grab webhook               └── Import webhook
                                                     │                        │
                                                     ▼                        ▼
                                              label_evidence (append-only, one row per event)
                                                     │
                     ┌───────────────────────────────┼─────────────────────────────┐
                     ▼                               ▼                             ▼
          torrent_canonical_labels        torrent_grab_attempts →           liveness
          (strongest evidence wins;       grab_outcome_priors               (alive / suspect / dead)
           preempts the classifier)       (Beta(α,β) per feature;                   │
                     │                     re-ranks Torznab)                        ▼
                     ▼                               │                    Torznab exclusion,
           classifier skipped or                     ▼                    DHT revalidation
           constrained on reprocess           Torznab result order
```

## Sources

| Source | What it sends | Transport | Config |
|---|---|---|---|
| Sonarr / Radarr / Readarr / Lidarr **webhook** | `Grab`, `Download`, `DownloadFolderImported` — the standard *Connect → Webhook* notification payload | `POST /evidence/arr/<instance>` with the shared secret in `X-Evidence-Token` | `EVIDENCE_WEBHOOK_SECRET` |
| \*arr **history poller** | The same grab/import events read back from `/api/v3/history`, as a backstop for lost deliveries | Outbound, on `EVIDENCE_ARR_POLL_INTERVAL` | `EVIDENCE_{SONARR,RADARR,READARR,LIDARR}_BASE_URL` / `_API_KEY` |
| **qBittorrent poller** | Category and tag per torrent (`private`, `public`, …) and torrent state (stalled, metaDL, seeding, …) | Outbound, on `EVIDENCE_QB_POLL_INTERVAL` | `EVIDENCE_QB_ALPHA_BASE_URL` / `_USERNAME` / `_PASSWORD` (and `QB_BETA`) |

The webhook is the primary, low-latency path and needs only that the \*arr can
reach BitAgent's HTTP port — the same reachability Torznab already requires. The
two pollers are optional: they need BitAgent to reach the \*arr and qBittorrent,
which a crawler behind a VPN may deliberately not have. Without them you lose the
lost-delivery backstop, the private-tracker category signal and the client-state
half of liveness; the grab → import loop still closes.

Every event, from every source, lands as one row in `label_evidence`. The table
is append-only and deduplicated on the source's own event id, so a webhook and
its poller echo count once.

## Configuring the \*arr webhook

Identical across Sonarr, Radarr, Lidarr and Readarr:

1. **Settings → Connect → + → Webhook**
2. **URL:** `http://<bitagent-host>:3333/evidence/arr/sonarr` (use `radarr`, `lidarr`, `readarr` for the others — the path segment names the instance)
3. **Method:** `POST`
4. **Triggers:** *On Grab*, *On Import*, *On Upgrade*. Leave the rest off; unknown events are acknowledged and dropped without a write.
5. **Custom Headers:** `X-Evidence-Token: <your EVIDENCE_WEBHOOK_SECRET>`
6. **Test**, then **Save**.

A test delivery returns `200 {"status":"ignored","event":"Test"}`. That is the
correct answer: the endpoint accepted and authenticated it, and `Test` is not
evidence. `bitagent_evidence_events_received_total{source,kind}` increments on
the first real grab.

## What the evidence becomes

### 1. A canonical label

`torrent_canonical_labels` holds one label per infohash, resolved from the
strongest applicable evidence row by a fixed precedence: an \*arr **import**
outranks an \*arr **grab**, which outranks a qBittorrent **private** category,
which outranks a **public** one. A label carries the media type (TV, movie,
book, music) and, when the \*arr supplied it, the external id of the series or
movie.

The classifier consults this table before it runs. A torrent with an
authoritative label is **preempted** — nothing to compute, the \*arr already
told us what it is — and a torrent with a type-only label is **constrained** so
the rules cannot re-type it. On a DHT-heavy workload most torrents have no label
yet (`bitagent_classifier_preempt_misses_total` is the expected steady state);
the shortcut matters on reprocess of things your \*arrs have already touched.

### 2. Success priors

Each grab opens a `torrent_grab_attempts` row carrying a frozen feature set
extracted from the release: release group, source tracker or indexer, primary
file extension, and a few more. When the matching import arrives, α is
incremented on every one of those features; when no import arrives inside
`EVIDENCE_OUTCOME_PRIORS_RESOLUTION_WINDOW`, the expirer increments β instead.
`grab_outcome_priors` therefore holds a Beta(α, β) prior per (feature type,
feature value): the observed success rate of releases with that property, from
*your* grabs.

On every Torznab search the ranker looks up the priors for each result's
features, computes a posterior expected success, weights it by `log(1 + seeders)`,
and sorts. With `EVIDENCE_OUTCOME_PRIORS_APPLY=false` it computes and records the
reordering it *would* make (`bitagent_priors_rerank_shift_positions`) but returns
the original order — read that histogram before flipping. With `APPLY=true` the
\*arr receives the re-ranked list. The \*arr's own quality profile still decides
what it takes; BitAgent only changes what it sees first.

Grabs from a private-tracker category are excluded from α by default
(`EVIDENCE_OUTCOME_PRIORS_EXCLUDE_PRIVATE_TRACKER_GRABS`): a private import
proves nothing about what the public swarm can serve.

### 3. Identity for torrents your \*arrs have not grabbed yet

A canonical label decides only the torrent it belongs to. With
`CLASSIFIER_EVIDENCE_TITLE_IDENTITY=true`, the labels also decide **other**
torrents released under the same title: when BitAgent classifies a torrent that
has no label of its own, it asks which catalogue identity the \*arrs gave the
torrents they *did* grab under that title — and, when at least two of them agree
and hold at least two thirds of the votes, it hints that identity to the
matcher.

This fixes the one kind of error a matcher cannot fix by itself: several
catalogue entries with the same name. *Married at First Sight* US and AU,
*iCarly* 2007 and its 2021 revival, *Kitchen Nightmares* UK and US look
identical to title matching. Your \*arr already chose one of them — it is the
series you monitor — and Sonarr searches Torznab by that series' id, so a
release attached to its namesake is a release Sonarr never sees.

The rules keep it conservative:

- **A torrent is never its own evidence.** Its own label, if any, is handled by
  the exact preempt above; the title vote only counts other torrents.
- **The release's shape is part of the title.** Episode-shaped names vote only
  with episode-shaped names; a movie keeps its year, so a *Dune (2021)* grab
  never steers a *Dune (1984)* torrent.
- **It only hints.** The workflow still resolves the identity against the
  catalogue and attaches it; `tvdb:` identities from Sonarr resolve through
  TMDB's external ids exactly as they do for the preempt.

On the 2026-09-22 benchmark it would have corrected 141 of 3,344 gold rows
that the deployed matcher gets wrong, without breaking one (93.7% → 97.9%) ([`docs/project/benchmarks.md`](project/benchmarks.md)).
Off by default; `bitagent_classifier_preempt_evidence_title_total{outcome}`
reports what it did.

### 4. Liveness

The liveness ladder derives `alive`, `suspect` or `dead` per infohash. An \*arr
import is the strongest alive signal there is — bytes reached disk. qBittorrent
state observations (`stalled`, `metaDL`, `error`) push toward suspect; a tracker
scrape that finds seeders revives a suspect row. Promotion to `dead` is gated
and rate-limited, and dead hashes are excluded from Torznab responses and
revalidated through DHT `get_peers` on a schedule. See
[`concepts/swarm-health.md`](concepts/swarm-health.md) for how tracker scrapes
feed this.

## Reading it

- **Evidence tab** in the dashboard: recent events per source, the canonical
  label a torrent resolved to, and per-indexer grab and import counts.
- **GraphQL:** `evidence { list(input:{limit:…}) { totalCount items { … } } }`
  and `evidence { indexerStats }`.
- **Metrics:** `bitagent_evidence_events_received_total{source,kind}`,
  `bitagent_evidence_events_persisted_total`, `bitagent_evidence_rejected_total{reason}`,
  `bitagent_evidence_canonical_upserts_total{source,media_type}`,
  `bitagent_classifier_preempt_preempted_total`, `bitagent_priors_observations_total{event}`,
  `bitagent_priors_outcomes_total{class}`, `bitagent_priors_pending_grabs`,
  `bitagent_priors_rerank_shift_positions`, `bitagent_liveness_*`.

The number that says whether the loop is paying for itself is
`priors_outcomes_total{class="success"}` over `success + failure`: the import
rate of what your \*arrs grabbed from BitAgent.

## Privacy

Every evidence row stays in your Postgres. Nothing is sent anywhere. Torrents
labelled as coming from a private tracker are additionally kept out of every
LLM stage and excluded from priors, so private-tracker activity never leaves
the host or biases public ranking. To reset, truncate `label_evidence`,
`torrent_canonical_labels`, `torrent_grab_attempts` and `grab_outcome_priors`.
