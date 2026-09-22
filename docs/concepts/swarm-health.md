# Swarm health

Seeder and leecher counts decide whether your *arr grabs a release, so BitAgent
reports them **only when it has a real measurement** — a release nobody has
measured is served with no `seeders` attribute at all rather than a made-up
number. Getting this wrong in one direction has Sonarr queue a dead swarm that
sits in `metaDL` until something reaps it; wrong in the other suppresses a fresh
release that is very much alive. The absence of a count is itself part of the
contract, and the rest of this page is about when it appears and what it means.

BitAgent produces those counts from two sources that are not equally good, and
most of this page is about telling them apart.

## Why DHT peer counts are not swarm counts

The crawler records seeders and leechers at discovery time from BEP-33 DHT
scrapes, stored as `torrents_torrent_sources(source='dht')`. That number is
cheap and it is always wrong in predictable ways:

- **It is one node's view.** A BEP-33 reply is a bloom filter of the peers *that
  node* knows about, not the swarm.
- **The seed flag is voluntary.** Peers that do not advertise as seeds are
  invisible to it, so the count is biased low.
- **It is quantized and it saturates.** Low counts round badly; the filter
  stops distinguishing above roughly 6000 peers.
- **It is effectively write-once.** Nothing refreshes it unless the crawler
  happens to re-encounter the same infohash organically. A count from March is
  still sitting there in September.

A zero from this source therefore means "I did not see anyone", not "nobody is
there" — and a young, healthy release reads zero more often than you would
expect.

## The tracker scrape

The `seeds` worker improves on that by asking the parties that actually keep a
register: public trackers, over BEP-15 UDP scrape. A tracker returns
`complete`/`incomplete`/`downloaded` counts for the peers currently announcing
**to it** — a census of a real population rather than one node's bloom estimate
of it — and does so at about seventy infohashes per datagram, so refreshing
millions of rows on a daily cadence costs trivial bandwidth.

That scope limit matters and is not hand-waved anywhere below: a tracker speaks
only for its own announce list. Peers reachable solely through the DHT, peer
exchange, or a tracker outside the pool are invisible to it. What a scrape buys
is a *measured, dated, re-checkable* reading from a named source — which is
strictly more than the DHT estimate offers, and still not omniscience.

The worker is **off by default**, and when first enabled it is also in
**dry-run**, because the useful number to learn before writing anything is
coverage: how much of your catalog public trackers know about at all.

### One cycle

1. **Select.** Take up to `SEEDS_BATCH_SIZE` infohashes that have never been
   scraped or whose last scrape is older than `SEEDS_MIN_RESCRAPE_AGE`,
   least-recently-checked first. Never-checked hashes sort ahead of stale ones.
2. **Scrape.** Fan out across the tracker pool, `SEEDS_CONCURRENCY` trackers at
   a time, chunking each batch into `SEEDS_MAX_HASHES_PER_PACKET` hashes per
   datagram and pacing packets to one tracker by `SEEDS_PER_TRACKER_INTERVAL`.
   A tracker that fails its first packet is abandoned for the cycle — it is
   almost certainly down or does not answer scrape — and the error is counted
   once rather than retried into a ban.
3. **Merge.** Take the **maximum** across the whole pool per hash, so a hash
   known to any single tracker is found. `best_tracker` records which one
   reported the winning seeder count. A tracker answering all zeros is treated
   as not knowing the hash, not as reporting a dead swarm.
4. **Persist.** Write the ledger row for every hash in the batch, including the
   ones no tracker knew — `checked_at` advances regardless, so an unknown hash is
   not re-scraped every single cycle.
5. **Sync.** Recompute the denormalized `torrent_contents.seeders/leechers` for
   the batch. Without this step the columns that Torznab ordering, the
   zero-seeder filter and the dashboard actually read stay frozen at classify
   time.
6. **Feed liveness.** Positive scrapes revive suspect rows in the evidence
   liveness ladder; authoritative zeros record a suspect observation. Promotion
   all the way to *dead* stays with the liveness revalidator — a scrape alone
   never condemns a torrent.

## Three states, not a number

A bare seeder integer cannot express the difference between "dead" and "I don't
know", so the ledger records the distinction explicitly:

| Verdict | Ledger row | Meaning |
|---|---|---|
| **Positive** | `tracker_known=true`, `seeders > 0` | A tracker reports live peers. Takes precedence over the DHT estimate. |
| **Known-zero** | `tracker_known=true`, `seeders = 0` | A tracker that knows this hash reports no seeders. Treated as a real zero — it is a measured absence from a source that has the hash, not a failure to look. |
| **Unknown** | `tracker_known=false`, `seeders NULL` | No tracker in the pool knows this hash. Honest unknown — **not** a zero. |

This is the distinction the whole subsystem exists to preserve. Collapsing
unknown into zero is what makes an indexer hide good releases.

## The ledger

`torrent_tracker_seeds` holds one row per infohash and is the durable
provenance record — what was checked, when, and by which tracker. Alongside the
current reading it maintains three history columns, updated inline on every
scrape:

| Column | What it gives you |
|---|---|
| `prev_seeders` | The previous reading, so current-minus-previous is per-interval velocity: is this swarm draining or growing? |
| `peak_seeders` | The highest authoritative count ever recorded, honestly labelled as history rather than masquerading as current. |
| `last_positive_at` | When the swarm was last seen alive. A known-zero row last positive six months ago is a very different object from one that seeded yesterday. |

History starts at each row's next scrape rather than being backfilled — a
metadata-cheap `UPDATE` across millions of rows inside the migration
transaction is a crash-loop hazard, and the ~daily cycle fills it in on its own.

## How the verdict reaches your *arr

A positive or known-zero verdict is mirrored into
`torrents_torrent_sources(source='tracker')`; a tracker-unknown verdict deletes
any stale row there. Downstream readers then need no special cases:

- **`Torrent.Seeders()`** returns the `tracker` row outright when one exists —
  including a tracker-reported `0` — and only falls back to max-across-sources
  when there is no tracker verdict. Before this precedence existed, a fossilised
  DHT peak permanently masked every fresher tracker reading.
- **`TORZNAB_HIDE_ZERO_SEEDERS`** drops dead swarms from search responses. With
  `TORZNAB_ZERO_SEEDERS_AUTHORITATIVE_ONLY` (the default) only a tracker-written
  zero counts; a bloom-only zero is treated as unknown instead of being
  suppressed.
- **Unknowns are served without a `seeders` attribute at all.** An *arr's
  `minimumSeeders` check then reads the release as unknown rather than
  rejecting it at zero. They age out through
  `TORZNAB_HIDE_UNKNOWN_SEEDERS_AGE_DAYS` instead, so a fresh DHT discovery
  still gets its chance.
- **`seedscheckedat`** is emitted as a Torznab attribute whenever a tracker
  verdict exists — an RFC3339 timestamp saying "a named tracker reported these
  counts at this time". Its absence is equally meaningful: these counts are
  DHT-approximate and deliberately unlabelled.

## Configuration

Worker settings, env prefix `SEEDS_`:

| Setting | Default | Notes |
|---|---|---|
| `SEEDS_ENABLED` | `false` | Starts the scheduled worker. |
| `SEEDS_ENABLE_WRITE` | `false` | Dry-run gate. Scrapes and emits metrics but writes nothing. Flip after reviewing coverage. |
| `SEEDS_INTERVAL` | `1h` | Between cycles. First cycle is offset 30s after boot so it does not pile onto startup. |
| `SEEDS_MIN_RESCRAPE_AGE` | `12h` | A hash checked more recently than this is skipped. Trackers only update swarm counts periodically; scraping faster wastes packets. |
| `SEEDS_BATCH_SIZE` | `2000` | Hashes per cycle. Size this against catalog size and interval so a full sweep lands on the cadence you want. |
| `SEEDS_CONCURRENCY` | `4` | Trackers scraped in parallel. |
| `SEEDS_MAX_HASHES_PER_PACKET` | `70` | Wire limit is ~74 (MTU / 20 bytes per hash); 70 leaves headroom. |
| `SEEDS_SCRAPE_TIMEOUT` | `15s` | One connect+scrape round trip. |
| `SEEDS_PER_TRACKER_INTERVAL` | `1s` | Politeness spacing per tracker. |
| `SEEDS_TRACKER_URLS` | built-in pool | Comma-separated UDP announce URLs. Trackers that are down or do not answer scrape are counted and skipped, so a generous list is fine. |

Torznab-side settings that read the verdict:

| Setting | Default | Notes |
|---|---|---|
| `TORZNAB_HIDE_ZERO_SEEDERS` | `true` | Drop confirmed-dead swarms from responses. |
| `TORZNAB_ZERO_SEEDERS_AUTHORITATIVE_ONLY` | `true` | Only a tracker-written zero is a real zero. Set `false` for pre-v0.41 behaviour, where a DHT bloom zero also counts. |
| `TORZNAB_HIDE_UNKNOWN_SEEDERS_AGE_DAYS` | `7` | Drop items that have no tracker verdict, no positive count from any source, and whose source rows are all older than this. `0` disables. |

## Running it on demand

```bash
# Measure first: scrape one batch, report coverage, write nothing.
bitagent refresh-seeds --dryRun

# Backfill: loop batches until the stale set drains.
bitagent refresh-seeds

# Bounded backfill.
bitagent refresh-seeds --limit 50000
```

Backfill is resumable — each batch stamps `checked_at`, so a re-run picks up
where the last one stopped once `SEEDS_MIN_RESCRAPE_AGE` has elapsed for the
earlier rows.

## What to watch

Coverage is the number that tells you whether this is working. Three gauges are
set at the end of every cycle:

| Metric | Reading |
|---|---|
| `bitagent_seeds_ledger_rows` | Hashes checked at least once. Against your total torrent count, this is sweep progress. |
| `bitagent_seeds_tracker_known_rows` | Hashes a tracker knows at all (positive or known-zero). Divided by ledger rows, this is your real coverage ceiling. |
| `bitagent_seeds_positive_rows` | Hashes currently carrying a live count. This is the fraction of your catalog that is actually grabbable. |

Per-cycle counters split the same way — `positive_total`, `known_zero_total`,
`unknown_total` — plus `sources_upserted_total` / `sources_cleared_total` for
write volume, `denorm_synced_total` for how much drift the sync is absorbing,
and `tracker_scrapes_total{tracker,result}` to spot a tracker that has stopped
answering. `cycle_errors_total{stage}` separates `select` / `scrape` / `persist`
/ `denorm` failures.

A large and growing `unknown` share is not a bug. Public trackers do not know
about every infohash on the DHT, and the honest answer to "how many seeders"
for those is *no answer*.

## Limits

- **A tracker speaks only for itself.** `complete`/`incomplete` count peers
  announcing to that tracker. A known-zero verdict means no announcing seeder on
  any tracker in the pool — it does not prove the swarm is globally dead, since
  peers can be reachable through DHT, PEX or a tracker you do not scrape.
  Hiding those results is an application policy (`TORZNAB_HIDE_ZERO_SEEDERS`),
  chosen because a *arr queueing a zero-announce swarm usually stalls in
  `metaDL`. Turn it off if you would rather try them.
- A scrape is a point-in-time reading on a cadence, not a live number. A swarm
  can die between the last scrape and your grab; `seedscheckedat` is there so
  you can see how old the verdict is.
- Coverage is bounded by the tracker pool. Hashes that only ever lived on
  private trackers or in the DHT stay unknown no matter how long you run.
- Trackers can be wrong or stale. The max-across-pool merge makes an inflated
  count from one tracker win, which is deliberate — a false positive costs you a failed grab,
  a false negative costs you a release you never saw.
- The worker never deletes a torrent. It produces verdicts; retention,
  junk-purge and the liveness ladder decide what to do with them.
