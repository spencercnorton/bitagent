-- +goose Up
-- +goose StatementBegin

-- Before v0.41 the seeds worker mirrored a 'tracker' source row only for
-- POSITIVE scrape results and deleted the row on a known-zero verdict, so an
-- authoritative dead swarm was indistinguishable in torrents_torrent_sources
-- from a never-scraped hash. The worker now upserts a 0/0 row for known-zero
-- (tracker_known AND seeders=0 AND leechers=0); this backfills rows for
-- hashes whose latest ledger verdict is already known-zero, so the torznab
-- authoritative-zero filter hides them from day one instead of waiting up to
-- a full re-scrape sweep (~2 days).
insert into torrents_torrent_sources
  (source, info_hash, seeders, leechers, created_at, updated_at)
select 'tracker', s.info_hash, 0, 0, now(), now()
from torrent_tracker_seeds s
where s.tracker_known
  and coalesce(s.seeders, 0) = 0
  and coalesce(s.leechers, 0) = 0
  and exists (select 1 from torrents t where t.info_hash = s.info_hash)
on conflict (info_hash, source) do nothing;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

delete from torrents_torrent_sources
where source = 'tracker' and seeders = 0 and leechers = 0;

-- +goose StatementEnd
