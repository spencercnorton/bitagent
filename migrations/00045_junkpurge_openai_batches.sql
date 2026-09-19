-- +goose Up
-- +goose StatementBegin

-- A run is one safety cohort. Provider expiry may split its work across
-- multiple attempts, but judgments and quarantine are applied only after the
-- full cohort has valid results. That preserves the existing whole-cycle
-- junk-rate circuit breaker.
create table junkpurge_batch_runs
(
  id                  bigint generated always as identity primary key,
  state               text        not null
    check (state in
      ('building','active','finalizing','completed','dry_run',
       'breaker_blocked','failed')),
  model               text        not null,
  provider_base_url   text        not null,
  endpoint            text        not null,
  prompt_version      text        not null,
  system_prompt       text        not null,
  max_completion_tokens integer   not null check (max_completion_tokens > 0),
  reasoning_effort    text        not null,
  completion_window   text        not null,
  min_age_seconds     bigint      not null check (min_age_seconds > 0),
  min_confidence      real        not null check (min_confidence > 0 and min_confidence <= 1),
  max_junk_rate       real        not null check (max_junk_rate > 0 and max_junk_rate <= 1),
  enable_purge        boolean     not null,
  quarantine_days     integer     not null check (quarantine_days > 0),
  failure_cooldown_seconds bigint not null check (failure_cooldown_seconds > 0),
  max_attempts        integer     not null check (max_attempts > 0),
  ambiguity_grace_seconds bigint  not null check (ambiguity_grace_seconds > 0),
  item_count          integer     not null default 0 check (item_count >= 0),
  failure_code        text,
  failure_message     text,
  created_at          timestamptz not null default now(),
  updated_at          timestamptz not null default now(),
  finalized_at        timestamptz
);

create index junkpurge_batch_runs_active_idx
  on junkpurge_batch_runs (created_at)
  where state in ('building','active','finalizing');

-- An attempt is one uploaded JSONL file and one provider Batch job. All
-- provider identifiers are persisted independently so a restarted worker can
-- reconcile ambiguous create timeouts rather than submit duplicate paid work.
create table junkpurge_batch_attempts
(
  id                       bigint generated always as identity primary key,
  run_id                   bigint      not null references junkpurge_batch_runs(id),
  attempt_no               integer     not null check (attempt_no > 0),
  state                    text        not null
    check (state in
      ('prepared','uploading','uploaded','submitting','validating',
       'in_progress','finalizing','ingesting','completed','failed',
       'expired','cancelling','cancelled')),
  input_filename           text        not null,
  input_sha256             text        not null,
  input_payload            bytea       not null,
  input_bytes              bigint      not null check (input_bytes >= 0),
  item_count               integer     not null check (item_count > 0),
  input_file_id            text,
  provider_batch_id        text,
  output_file_id           text,
  error_file_id            text,
  provider_status          text,
  request_total            integer     not null default 0,
  request_completed        integer     not null default 0,
  request_failed           integer     not null default 0,
  submission_attempted_at  timestamptz,
  submitted_at             timestamptz,
  terminal_at              timestamptz,
  ingested_at              timestamptz,
  next_attempt_at          timestamptz,
  lease_owner              text,
  lease_until              timestamptz,
  last_error               text,
  created_at               timestamptz not null default now(),
  updated_at               timestamptz not null default now(),
  unique (run_id, attempt_no),
  unique (input_file_id),
  unique (provider_batch_id),
  check (request_total >= 0 and request_completed >= 0 and request_failed >= 0)
);

create index junkpurge_batch_attempts_reconcile_idx
  on junkpurge_batch_attempts (next_attempt_at, created_at)
  where ingested_at is null;

-- custom_id is the only safe result join key because provider output order is
-- unspecified. Per-item results and token usage remain after provider files
-- are cleaned up, giving BitAgent its own auditable cost ledger.
create table junkpurge_batch_items
(
  run_id                 bigint      not null references junkpurge_batch_runs(id),
  ordinal                integer     not null check (ordinal > 0),
  custom_id              text        not null unique,
  info_hash              bytea       not null check (octet_length(info_hash) = 20),
  torrent_name           text        not null,
  state                  text        not null
    check (state in
      ('pending','submitted','succeeded','retryable_error',
       'terminal_error','applied','abandoned')),
  attempt_count          integer     not null default 0 check (attempt_count >= 0),
  last_attempt_id        bigint references junkpurge_batch_attempts(id),
  verdict                text,
  confidence             real,
  provider_request_id    text,
  response_status        integer,
  error_code             text,
  error_message          text,
  retry_after            timestamptz,
  input_tokens           bigint      not null default 0,
  cached_input_tokens    bigint      not null default 0,
  cache_write_tokens     bigint      not null default 0,
  output_tokens          bigint      not null default 0,
  reasoning_tokens       bigint      not null default 0,
  completed_at           timestamptz,
  created_at             timestamptz not null default now(),
  updated_at             timestamptz not null default now(),
  primary key (run_id, ordinal),
  unique (run_id, info_hash),
  check (confidence is null or (confidence >= 0 and confidence <= 1))
);

-- One active run owns a hash at a time. Terminal run finalization moves every
-- item to applied/abandoned, releasing failed items for a future cohort while
-- successful items remain suppressed by junkpurge_judgments.
create unique index junkpurge_batch_items_active_hash_idx
  on junkpurge_batch_items (info_hash)
  where state in ('pending','submitted','succeeded','retryable_error');

create index junkpurge_batch_items_run_state_idx
  on junkpurge_batch_items (run_id, state);

create index junkpurge_batch_items_retry_after_idx
  on junkpurge_batch_items (info_hash, retry_after)
  where state = 'abandoned';

-- Standard (non-provider-Batch) cycles still need a durable per-hash claim:
-- SELECT ... FOR UPDATE issued through a pool releases its locks before the
-- slow LLM call. This lease prevents multiple replicas from paying for the
-- same title and gates destructive settlement to the worker that judged it.
create table junkpurge_sync_claims
(
  info_hash     bytea       primary key references torrents(info_hash) on delete cascade
    check (octet_length(info_hash) = 20),
  owner         text        not null,
  torrent_name  text        not null,
  claimed_at    timestamptz not null default now(),
  lease_until   timestamptz not null,
  check (lease_until > claimed_at)
);

create index junkpurge_sync_claims_lease_idx
  on junkpurge_sync_claims (lease_until);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists junkpurge_sync_claims;
drop table if exists junkpurge_batch_items;
drop table if exists junkpurge_batch_attempts;
drop table if exists junkpurge_batch_runs;

-- +goose StatementEnd
