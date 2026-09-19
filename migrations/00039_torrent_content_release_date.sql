-- +goose Up
-- +goose StatementBegin

-- release_date is the per-release air date parsed from the torrent name
-- (parsers.ParseDate via the classifier's parse_date action), persisted so
-- daily-show Torznab queries (season=YYYY&ep=MM/DD) can match on date instead
-- of being silently dropped. NULL = no full date in the name (or a row
-- classified before this migration that the backfill has not reached).
--
-- DELIBERATELY no backfill here (same reasoning as 00037): migrations run in
-- goose's single startup transaction before :3333 binds, and a long UPDATE
-- would be killed by the healthcheck+autoheal contract. Existing rows are
-- populated by the `derived-backfill` command; new classifications are
-- stamped inline by the processor. Adding a nullable column with no default
-- is metadata-only in Postgres 11+ — instant.
alter table torrent_contents
  add column release_date date;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table torrent_contents drop column if exists release_date;

-- +goose StatementEnd
