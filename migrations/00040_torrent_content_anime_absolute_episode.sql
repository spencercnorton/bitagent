-- +goose Up
-- +goose StatementBegin

-- anime_absolute_episode persists the absolute episode number the anime
-- detector (internal/anime.Detect) parses from the canonical fansub shape
-- "[Group] Title - NNN" — previously computed on every classification and
-- discarded into the is_anime bool. NULL = not an anime release or no
-- absolute number in the name. Only stamped when the full anime signal fires
-- (is_anime), so bare dash-number names ("Show - 320") cannot leak noise in.
--
-- No backfill here (same startup-transaction rationale as 00037/00039);
-- existing rows are populated by `derived-backfill`. Nullable column with no
-- default: metadata-only, instant.
alter table torrent_contents
  add column anime_absolute_episode integer;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table torrent_contents drop column if exists anime_absolute_episode;

-- +goose StatementEnd
