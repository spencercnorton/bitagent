-- +goose Up
-- +goose StatementBegin
-- Independent review projection. No consumer or deletion query reads this
-- table. One bounded receipt per public hash; observations are capped in Go.
create table catalogue_policy_shadow (
  info_hash bytea primary key check (octet_length(info_hash) = 20),
  input_digest text not null default '',
  policy_version text not null default '',
  receipt jsonb not null default '{}'::jsonb,
  evaluated_at timestamptz not null default now(),
  expires_at timestamptz not null default now()
);
create index catalogue_policy_shadow_expiry_idx on catalogue_policy_shadow (expires_at);
-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
drop table catalogue_policy_shadow;
-- +goose StatementEnd
