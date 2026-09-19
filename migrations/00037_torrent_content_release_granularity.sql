-- +goose Up
-- +goose StatementBegin

-- release_granularity classifies how much of a TV series a release carries
-- (episode | multi_episode | partial_season | season | multi_season |
-- complete_series), derived at classify time by
-- model.DeriveReleaseGranularity from the parsed episodes shape and the
-- release name. NULL = unknown (non-TV, or TV with no episode information
-- and no explicit series claim, or a row classified before this migration
-- that the backfill has not reached yet). Plain nullable text like
-- content_type — no CHECK constraint, the writer is the single source of
-- truth.
--
-- DELIBERATELY no backfill here. Migrations run inside goose's single
-- startup transaction BEFORE :3333 binds, and the prod container is
-- healthchecked on :3333 with autoheal enabled — a multi-minute UPDATE over
-- ~1.1M tv rows would be SIGTERMed mid-transaction at ~t+4min, roll back,
-- and re-run from zero on every restart (measured ≥6.5min on prod-sized
-- data: a guaranteed crash loop, same reason 00036 used NO TRANSACTION for
-- its long work). Existing rows are populated by the one-off
-- `granularity-backfill` command (dry-run default, batched, resumable,
-- UpdateColumn so updated_at is untouched); newly classified rows are
-- stamped inline by the processor. Adding a nullable column with no default
-- is metadata-only in Postgres 11+ — instant, no table rewrite.
alter table torrent_contents
  add column release_granularity text;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table torrent_contents drop column if exists release_granularity;

-- +goose StatementEnd
