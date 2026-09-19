-- +goose Up
-- eval_frozen holds immutable evaluation corpora for the classifier eval
-- harness (roadmap P0/WI0.1). Each row pins a torrent into a named segment,
-- freezing its name at freeze time so later renames/deletes cannot shift the
-- corpus. expected carries optional ground truth (e.g. resolved TMDB id from
-- arr canonical labels) as jsonb.
create table eval_frozen
(
  segment   text                     not null,
  info_hash bytea                    not null,
  name      text                     not null default '',
  expected  jsonb,
  added_at  timestamp with time zone not null default now(),
  primary key (segment, info_hash)
);

-- +goose Down
drop table eval_frozen;
