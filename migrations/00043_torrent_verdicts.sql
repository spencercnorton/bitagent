-- +goose Up
-- +goose StatementBegin

-- T3 phase A (docs/design/verdict-ledger.md): the verdict
-- ledger. torrent_verdict_events is APPEND-ONLY — the truth; rows are never
-- updated or deleted, and info_hash carries NO FK so events survive torrent
-- deletion (tombstones/blacklists must outlive the row they judged).
-- torrent_verdict_state is the derived one-row-per-hash current state,
-- written in the same transaction as its event and rebuildable from events.
--
-- Phase A is dual-write only: junkpurge (and the operator restore path)
-- record verdicts alongside their existing bookkeeping. NOTHING reads these
-- tables yet — readers arrive in phase B behind a shadow-compared flag.
create table torrent_verdict_events
(
  id         bigint generated always as identity primary key,
  info_hash  bytea       not null check (octet_length(info_hash) = 20),
  verdict    text        not null check (verdict in
    ('active','suspect','quarantined','blacklisted','tombstoned','restored')),
  mechanism  text        not null check (mechanism in
    ('classifier_delete','content_filter','crawler_drop','junkpurge',
     'retention','blocking','csam','operator','liveness')),
  reason     text        not null,
  evidence   jsonb,
  actor      text        not null default 'system',
  -- expires_at rides on the EVENT too (not just derived state): events are
  -- the truth, and a rebuild must reproduce quarantine review windows.
  expires_at timestamptz,
  created_at timestamptz not null default now()
);

create index torrent_verdict_events_hash_idx
  on torrent_verdict_events (info_hash, created_at desc);

create table torrent_verdict_state
(
  info_hash  bytea primary key check (octet_length(info_hash) = 20),
  verdict    text        not null check (verdict in
    ('active','suspect','quarantined','blacklisted','tombstoned','restored')),
  mechanism  text        not null check (mechanism in
    ('classifier_delete','content_filter','crawler_drop','junkpurge',
     'retention','blocking','csam','operator','liveness')),
  since      timestamptz not null,
  expires_at timestamptz,
  updated_at timestamptz not null default now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists torrent_verdict_state;
drop table if exists torrent_verdict_events;

-- +goose StatementEnd
