-- +goose Up
-- +goose StatementBegin

-- english_audio_source records which layer wrote english_audio, so the two
-- unconditional writers (processor upsert with OnConflict UpdateAll, and
-- derived-backfill's full semantic diff) can enforce precedence instead of
-- silently erasing the LLM tier's output (the tripwire documented on
-- model.DeriveEnglishAudio):
--   'name' -- deterministic release-name signals (model.DeriveEnglishAudio)
--   'llm'  -- the llmmatch extraction (dub|sub|none), assignable only when
--            the deterministic layer had no explicit signal
-- Precedence: a fresh deterministic value always wins ('name' overwrites
-- 'llm'); an absent deterministic signal never downgrades an 'llm' row.
-- NULL iff english_audio is NULL -- enforced by construction (both writers
-- route through one merge in Go); no paired CHECK so this ALTER stays
-- instant on 2.1M rows.
alter table torrent_contents
  add column english_audio_source text
  constraint torrent_contents_english_audio_source_check
    check (english_audio_source in ('name', 'llm'));

-- Stamp pre-existing values 'name': the deterministic layer is the only
-- writer that has ever populated english_audio (~14.5K rows at migration
-- time). One bounded seq scan updating a few thousand rows -- seconds, not
-- the minutes-scale derive backfills that 00037/00039/00040/00042 kept out
-- of the startup transaction, so it is safe inline here; without it, legacy
-- rows would be indistinguishable from unclassified ones in provenance
-- queries. 'none' is excluded because the deterministic layer can never
-- produce it (LLM-only value): on first application there are zero such
-- rows, and on a down+re-up after the LLM has written, this stops the stamp
-- from relabelling surviving LLM 'none' values as 'name' -- which the next
-- derived-backfill would then clear. (Down drops only the source column;
-- surviving dub/sub values are re-stamped 'name' and so re-enter the
-- deterministic layer's normal self-correction.)
update torrent_contents
  set english_audio_source = 'name'
  where english_audio is not null and english_audio <> 'none';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table torrent_contents drop column if exists english_audio_source;

-- +goose StatementEnd
