-- +goose Up
-- +goose StatementBegin

-- junkpurge_quarantine is the holding area for confident-junk torrents the
-- junk-purge worker has removed from the main DB but not yet permanently
-- deleted. Instead of a hard DELETE, the worker snapshots the raw torrent row
-- (torrent_snapshot) and its file list (files_snapshot) here, then deletes the
-- torrent — so it leaves search/Torznab immediately but stays fully restorable
-- for the review window (default 30 days). After the window the worker
-- permanently deletes the row and blacklists the info_hash.
--
-- Snapshots are stored as jsonb (to_jsonb of the rows) so the table is immune
-- to schema drift in torrents / torrent_files. No FK to torrents — the whole
-- point is that the torrent is gone from the main tables while this remains.
create table junkpurge_quarantine
(
  info_hash        bytea       primary key,
  torrent_name     text        not null,
  verdict          text        not null,
  confidence       real        not null,
  quarantined_at   timestamptz not null default now(),
  torrent_snapshot jsonb       not null,
  files_snapshot   jsonb
);

create index junkpurge_quarantine_quarantined_at_idx
  on junkpurge_quarantine (quarantined_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists junkpurge_quarantine;

-- +goose StatementEnd
