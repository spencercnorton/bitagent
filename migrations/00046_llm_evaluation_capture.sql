-- +goose Up
-- +goose StatementBegin

-- Prospective, disabled-by-default full-fidelity evaluation inputs. Raw
-- info_hash values are deliberately absent from the model-visible capture
-- rows: source/group aliases are namespaced SHA-256 values created only after
-- native and qB/bitgrab privacy admission. Captures are immutable apart from
-- bounded rolling retention.
create table llm_evaluation_captures
(
  id                       bigint generated always as identity primary key,
  capture_key              bytea       not null unique
    check (octet_length(capture_key) = 32),
  task                     text        not null
    check (task in (
      'matcher_extract','matcher_rerank','contentfilter','junkpurge'
    )),
  candidate_source         text
    check (candidate_source in ('local','api')),
  source_sha256            bytea       not null
    check (octet_length(source_sha256) = 32),
  group_sha256             bytea       not null
    check (octet_length(group_sha256) = 32),
  input_sha256             bytea       not null
    check (octet_length(input_sha256) = 32),
  contract_sha256          bytea       not null
    check (octet_length(contract_sha256) = 32),
  prompt_sha256            bytea       not null
    check (octet_length(prompt_sha256) = 32),
  endpoint_sha256          bytea       not null
    check (octet_length(endpoint_sha256) = 32),
  model                    text        not null check (model <> ''),
  prompt_version           text        not null check (prompt_version <> ''),
  build_identity           text        not null check (build_identity <> ''),
  contract_id              text        not null check (contract_id <> ''),
  system_prompt            text        not null check (system_prompt <> ''),
  model_input              jsonb       not null
    check (jsonb_typeof(model_input) = 'object'),
  task_input               jsonb       not null
    check (jsonb_typeof(task_input) = 'object'),
  capture_schema_version   integer     not null check (capture_schema_version = 1),
  privacy_status           text        not null
    check (privacy_status = 'verified_native_public_qb_rechecked'),
  sampling_origin          text        not null
    check (sampling_origin in (
      'natural_capture','safety_topup_capture'
    )),
  captured_at              timestamptz not null,
  expires_at               timestamptz not null,
  privacy_checked_at       timestamptz not null,
  check (expires_at > captured_at),
  check (
    (task = 'matcher_rerank' and candidate_source is not null)
    or
    (task <> 'matcher_rerank' and candidate_source is null)
  ),
  check (
    octet_length(model_input::text) + octet_length(task_input::text)
    <= 1048576
  )
);

create index llm_evaluation_captures_task_order_idx
  on llm_evaluation_captures (task, candidate_source, capture_key);

create index llm_evaluation_captures_expiry_idx
  on llm_evaluation_captures (expires_at);

create index llm_evaluation_captures_retention_order_idx
  on llm_evaluation_captures (captured_at desc, id desc);

create index llm_evaluation_captures_group_idx
  on llm_evaluation_captures (task, group_sha256);

-- Local admission sidecar. The raw info hash is required only to re-run the
-- live native/qB/bitgrab gate immediately before a future hosted request.
-- Exporters must replace it with case_id and write any BTIH mapping as a
-- separate mode-0600 artifact; it is never part of candidate/gold JSONL.
create table llm_evaluation_capture_admissions
(
  capture_key bytea       primary key
    references llm_evaluation_captures(capture_key) on delete cascade
    check (octet_length(capture_key) = 32),
  info_hash   bytea       not null check (octet_length(info_hash) = 20),
  expires_at  timestamptz not null
);

create index llm_evaluation_capture_admissions_hash_idx
  on llm_evaluation_capture_admissions (info_hash);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists llm_evaluation_capture_admissions;
drop table if exists llm_evaluation_captures;

-- +goose StatementEnd
