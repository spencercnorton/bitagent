-- +goose Up
-- +goose StatementBegin
create table catalogue_recovery_budget (
  singleton boolean primary key default true check(singleton),
  payload_bytes bigint not null default 0 check(payload_bytes >= 0),
  snapshots bigint not null default 0 check(snapshots >= 0)
);
insert into catalogue_recovery_budget(singleton) values(true);
create table catalogue_recovery_snapshots (
  id bigserial primary key,
  info_hash bytea not null check(octet_length(info_hash)=20),
  source_digest text not null,
  contract_version text not null,
  state text not null check(state in ('prepared','removed','restored')),
  reason text not null,
  snapshot jsonb not null,
  payload_bytes bigint not null check(payload_bytes > 0),
  created_at timestamptz not null,
  expires_at timestamptz not null,
  restored_at timestamptz,
  removed_verdict_event_id bigint references torrent_verdict_events(id),
  restored_verdict_event_id bigint references torrent_verdict_events(id),
  check(expires_at > created_at)
);
create index catalogue_recovery_hash_idx on catalogue_recovery_snapshots(info_hash);
create index catalogue_recovery_expiry_idx on catalogue_recovery_snapshots(expires_at);
create table catalogue_recovery_events (
  id bigserial primary key,
  snapshot_id bigint not null references catalogue_recovery_snapshots(id),
  transition text not null check(transition in ('prepared','removed','restored')),
  observed_at timestamptz not null,
  reason text not null
);
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
drop table catalogue_recovery_events;
drop table catalogue_recovery_snapshots;
drop table catalogue_recovery_budget;
-- +goose StatementEnd
