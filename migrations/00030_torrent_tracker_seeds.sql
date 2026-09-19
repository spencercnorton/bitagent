-- +goose Up
-- +goose StatementBegin

-- torrent_tracker_seeds is the authoritative seed/leech ledger produced by the
-- BEP-15 UDP tracker-scrape worker (internal/seeds). One row per infohash.
--
-- Motivation: the seeders/leechers on torrents_torrent_sources(source='dht')
-- are a single-node BEP-33 bloom *approximation* captured at crawl time and
-- never refreshed unless the crawler organically re-encounters the hash — so
-- they are biased low (the BEP-33 seed flag is voluntary), quantized at low
-- counts, saturated around 6000, and unboundedly stale. Public trackers return
-- authoritative complete/incomplete counts for the whole swarm. This worker
-- scrapes a pool of public UDP trackers and records the best (max across
-- trackers) result here.
--
-- The ledger deliberately distinguishes three states a bare seeders integer
-- cannot (Spencer's honest-unknown principle):
--   * positive    : tracker_known=true, seeders/leechers > 0  (authoritative live)
--   * known-zero  : tracker_known=true, seeders = 0           (a tracker has the
--                   hash but the swarm is dead — a real 0, not "unknown")
--   * unknown     : tracker_known=false, seeders NULL         (no tracker in the
--                   pool knows this hash; honest unknown, NOT a 0)
-- checked_at drives re-scrape scheduling independently of whether anything was
-- found, so an unknown hash is not re-scraped every cycle.
--
-- Surfacing: on a positive result the worker ALSO upserts
-- torrents_torrent_sources(source='tracker'), so the existing
-- Torrent.Seeders() max-across-sources aggregation, the Torznab adapter and
-- GraphQL pick up the authoritative number with no change to those code paths.
-- A non-positive result clears any stale 'tracker' source row. This ledger is
-- the durable provenance record and the freshness source of truth.
create table torrent_tracker_seeds
(
  info_hash     bytea       primary key references torrents on delete cascade,
  tracker_known boolean     not null default false,
  seeders       integer,
  leechers      integer,
  completed     integer,
  best_tracker  text,
  checked_at    timestamptz not null,
  created_at    timestamptz not null default now()
);

-- The worker selects the least-recently-checked hashes each cycle; this index
-- keeps the "order by checked_at" batch selection cheap.
create index torrent_tracker_seeds_checked_at_idx
  on torrent_tracker_seeds (checked_at);

-- Register the 'tracker' source so the FK on torrents_torrent_sources.source
-- (references torrent_sources on delete cascade) is satisfied when the worker
-- upserts authoritative rows.
insert into torrent_sources (key, name, created_at, updated_at)
values ('tracker', 'Tracker Scrape', now(), now())
on conflict (key) do nothing;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

delete from torrents_torrent_sources where source = 'tracker';
delete from torrent_sources where key = 'tracker';
drop table if exists torrent_tracker_seeds;

-- +goose StatementEnd
