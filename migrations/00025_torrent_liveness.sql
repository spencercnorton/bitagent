-- +goose Up
-- +goose StatementBegin

-- torrent_liveness is the per-infohash health view derived from
-- qBittorrent state observations and *arr import outcomes. One row
-- per infohash. The liveness resolver upserts here on every relevant
-- Evidence insert; the Torznab adapter consults it to drop dead
-- entries from search responses, and the revalidator worker re-tests
-- dead entries via DHT get_peers on a TTL.
--
-- This table does NOT replace the seeders/leechers fields on
-- torrent_contents — those reflect what the catalog ingested at
-- crawl time and may be months stale. Liveness is the live signal
-- bitagent has actually exercised by trying to download.
create table torrent_liveness
(
  info_hash             bytea       primary key,
  status                text        not null check (status in ('alive','suspect','dead')),
  last_qb_state         text,
  suspect_first_seen_at timestamptz,
  suspect_observations  integer     not null default 0,
  last_observed_at      timestamptz not null,
  blacklisted_at        timestamptz,
  next_revalidate_at    timestamptz,
  alive_source          text,
  created_at            timestamptz not null default now(),
  updated_at            timestamptz not null default now()
);

create index torrent_liveness_status_idx
  on torrent_liveness (status);

-- Partial index: the revalidator worker scans for dead entries whose
-- TTL has expired. Only those rows are interesting; a partial index
-- keeps the scan cheap even when the dead set is large.
create index torrent_liveness_revalidate_idx
  on torrent_liveness (next_revalidate_at)
  where status = 'dead' and next_revalidate_at is not null;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists torrent_liveness;

-- +goose StatementEnd
