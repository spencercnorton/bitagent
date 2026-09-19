-- +goose Up
-- +goose StatementBegin

-- english_audio classifies an ANIME release's English availability
-- (dub | sub | none), derived deterministically from release-name conventions
-- at classify time (model.DeriveEnglishAudio); a later phase adds the LLM
-- extraction with explicit precedence. NULL = unknown or not anime — Western
-- releases are English-audio by convention, so the distinction only carries
-- signal on is_anime rows. 'none' (a raw) is deliberately NOT derived
-- deterministically: "\braw\b" collides with fansub group names (Ohys-Raws,
-- Beatrice-Raws), so raws stay NULL until the LLM phase.
--
-- No backfill here (same startup-transaction rationale as 00037/00039/00040);
-- existing rows are populated by `derived-backfill`. Nullable, no default:
-- metadata-only, instant.
alter table torrent_contents
  add column english_audio text;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table torrent_contents drop column if exists english_audio;

-- +goose StatementEnd
