-- +goose Up
-- +goose StatementBegin

-- anime_titles is the deterministic anime-alias backbone. Every known anime
-- alias (romaji, kanji, English, synonym, short title) is normalized to a
-- lookup key and mapped to its canonical TMDB id + content type. It is built by
-- joining the AniDB title dump (anidbid -> many aliases) with the Anime-Lists
-- anime-list-full.xml (anidbid -> TMDB id + type), so the mapping is derived
-- entirely from vetted cross-reference data — no LLM world-knowledge and no
-- hardcoded per-title switches.
--
-- Purpose: drive the classifier's romaji/AKA resolution deterministically. A
-- messy romaji release name ("Shingeki no Kyojin", "KiseKoi") resolves straight
-- to a TMDB id via this table, retiring the applyLLMMatchEdgeOverrides /
-- forcedLLMMatchID switches that used to live in internal/classifier/matcher.go.
--
-- This is a PURE DERIVED CACHE. The animedb refresh worker (and the
-- `refresh-anime-titles` CLI) rebuild it wholesale — TRUNCATE + repopulate in
-- one transaction — from the upstream data files. It carries no authored state,
-- so dropping or truncating it is always safe; the next refresh restores it.
--
-- `normalized` is the primary key: aliases are deduplicated at build time so
-- each normalized form maps to exactly one (tmdb_type, tmdb_id). Any alias that
-- would map to MORE THAN ONE distinct TMDB entry is dropped at build time
-- (ambiguous) rather than persisted, so a lookup never has to disambiguate.
create table anime_titles
(
  normalized    text        primary key,
  tmdb_type     text        not null,          -- 'movie' | 'tv_show' (model.ContentType)
  tmdb_id       bigint      not null,
  anidb_id      integer     not null,
  display_title text        not null,          -- canonical/display form of the matched alias
  title_source  text        not null,          -- 'primary' | 'official' | 'synonym' | 'short'
  updated_at    timestamptz not null default now()
);

-- Exact-match lookups hit the primary-key index on `normalized`. A secondary
-- index on the TMDB target supports reverse lookups and coverage queries
-- ("how many aliases point at this show").
create index anime_titles_tmdb_idx on anime_titles (tmdb_type, tmdb_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists anime_titles;

-- +goose StatementEnd
