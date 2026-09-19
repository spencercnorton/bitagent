-- +goose Up
-- +goose StatementBegin

-- junkpurge_judgments records the local-LLM verdict for each unmatched
-- movie/tv torrent the junk-purge worker has evaluated. It exists so the
-- worker (a) does NOT re-spend LLM calls re-judging the same ~800k
-- persistently-unmatched-but-real torrents every cycle — once judged, an
-- info_hash is skipped until rejudge_interval elapses — and (b) produces an
-- auditable drop-list (verdict = 'junk') that can be reviewed BEFORE
-- enable_purge is flipped from its dry-run default.
--
-- verdict is the LLM's name-based call, independent of TMDB reachability:
--   'junk'         — spam / fake / mislabeled non-media; safe to delete
--   'real_mangled' — a real movie/show whose name TMDB couldn't match (keep)
--   'real_absent'  — a real movie/show TMDB genuinely lacks (keep)
--   'unsure'       — low confidence (keep)
create table junkpurge_judgments
(
  info_hash    bytea       primary key,
  verdict      text        not null,
  confidence   real        not null,
  reason       text,
  torrent_name text,
  judged_at    timestamptz not null default now(),
  purged       boolean     not null default false
);

create index junkpurge_judgments_verdict_idx on junkpurge_judgments (verdict);
create index junkpurge_judgments_judged_at_idx on junkpurge_judgments (judged_at desc);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists junkpurge_judgments;

-- +goose StatementEnd
