-- +goose Up
-- +goose StatementBegin

-- is_anime is a persisted, per-release anime signal on torrent_contents,
-- derived from the torrent name by internal/anime.Detect(name).IsAnime() at
-- classify time. It replaces the leading-bracket prefix-LIKE approximation
-- (the old TorrentContentAnimeCriteria) with the FULL detector result: a
-- known fansub-group bracket ANYWHERE in the name, an unlisted leading Latin
-- group tag combined with an absolute "Title - NNN" episode / romaji season
-- marker / English-track marker. The signal is computed once at classification
-- rather than recomputed from torrents.name on every cat=5070 query, and gives
-- a durable anime flag for future features (per-fansub scoring, dedup).
--
-- Adding a NOT NULL column with a constant default is metadata-only in
-- Postgres 11+ — no table rewrite, existing rows read the default without
-- being touched. Rows are seeded false here and corrected to their true value
-- by the one-off `anime-backfill` command (detection is Go, not SQL); newly
-- classified torrents get the correct value inline via the classifier.
--
-- No index is added: the only column-scan consumer is the rare bare cat=5070
-- browse, which is already narrowed by content_type (served by the existing
-- torrent_contents_content_type_updated_at_idx) before is_anime is applied as
-- a residual filter. A partial index (WHERE is_anime) is a reasonable future
-- follow-up, best built CONCURRENTLY once the backfill has populated the flag.
alter table torrent_contents
  add column is_anime boolean not null default false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table torrent_contents drop column if exists is_anime;

-- +goose StatementEnd
