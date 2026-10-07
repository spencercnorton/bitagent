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
-- Serialize against recovery writers before checking the empty-only downgrade.
-- Restored and expired snapshots remain retained history and consume budgets.
lock table catalogue_recovery_budget, catalogue_recovery_snapshots,
  catalogue_recovery_events in access exclusive mode;
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM catalogue_recovery_snapshots)
     OR EXISTS (SELECT 1 FROM catalogue_recovery_events)
     OR EXISTS (SELECT 1 FROM catalogue_recovery_budget
                WHERE payload_bytes <> 0 OR snapshots <> 0) THEN
    RAISE EXCEPTION 'catalogue recovery history and budgets must be retained; restore a matching backup before downgrade';
  END IF;
END $$;
drop table catalogue_recovery_events;
drop table catalogue_recovery_snapshots;
drop table catalogue_recovery_budget;
-- +goose StatementEnd
