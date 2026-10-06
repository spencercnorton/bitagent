-- +goose Up
-- Mutable task lifecycle is separate from immutable first-response evidence.
CREATE TABLE llm_work_tasks (
    task_key bytea PRIMARY KEY CHECK (octet_length(task_key)=32),
    kind text NOT NULL CHECK (kind IN ('classifier_type','contentfilter','matcher')),
    info_hash bytea NOT NULL CHECK (octet_length(info_hash)=20),
    source_digest bytea NOT NULL CHECK (octet_length(source_digest)=32),
    policy_digest bytea NOT NULL CHECK (octet_length(policy_digest)=32),
    input_digest bytea NOT NULL CHECK (octet_length(input_digest)=32),
    family_digest bytea NOT NULL CHECK (octet_length(family_digest)=32),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload)='object' AND octet_length(payload::text)<=65536),
    priority integer NOT NULL CHECK (priority BETWEEN 0 AND 100),
    time_bucket integer NOT NULL CHECK (time_bucket BETWEEN 0 AND 23),
    daily_limit integer NOT NULL CHECK (daily_limit>=0),
    monthly_limit integer NOT NULL CHECK (monthly_limit>=0),
    state text NOT NULL CHECK (state IN ('queued','deferred','leased','completed','held','obsolete','expired')),
    reason text NOT NULL DEFAULT '' CHECK (length(reason)<=64),
    created_at timestamptz NOT NULL DEFAULT now(),
    retry_after timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    lease_owner text,
    lease_generation bigint NOT NULL DEFAULT 0,
    lease_until timestamptz,
    completed_at timestamptz,
    CHECK ((state='leased')=(lease_owner IS NOT NULL AND lease_until IS NOT NULL)),
    CHECK (expires_at>created_at)
);
CREATE INDEX llm_work_tasks_ready_idx ON llm_work_tasks (state,retry_after,priority DESC,time_bucket,created_at);
CREATE INDEX llm_work_tasks_family_idx ON llm_work_tasks (kind,family_digest,state);
CREATE INDEX llm_work_tasks_source_idx ON llm_work_tasks (info_hash,kind,state);
CREATE TABLE llm_work_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_key bytea NOT NULL REFERENCES llm_work_tasks(task_key),
    state text NOT NULL,
    reason text NOT NULL CHECK (length(reason)<=64),
    happened_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX llm_work_events_task_idx ON llm_work_events (task_key,id);

-- Committed application evidence survives ordinary HTTP-body retention. It is
-- model application provenance, never a canonical label or human gold. Current
-- source, policy and committed target must still match before preservation.
CREATE TABLE llm_work_applications (
    task_key bytea PRIMARY KEY REFERENCES llm_work_tasks(task_key),
    info_hash bytea NOT NULL CHECK (octet_length(info_hash)=20),
    source_digest bytea NOT NULL CHECK (octet_length(source_digest)=32),
    policy_digest bytea NOT NULL CHECK (octet_length(policy_digest)=32),
    applied_snapshot jsonb NOT NULL CHECK (
      jsonb_typeof(applied_snapshot)='object' AND octet_length(applied_snapshot::text)<=65536),
    applied_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX llm_work_applications_source_idx ON llm_work_applications(info_hash);

-- Digest-only dispatch fences intentionally have NO foreign key to capture
-- retention or source-case jobs. Raw request/response/body/hash data is not here.
CREATE TABLE llm_capture_dispatch_attempts (
 capture_key bytea PRIMARY KEY CHECK (octet_length(capture_key)=32),
 task text NOT NULL CHECK (task IN ('matcher_extract','matcher_rerank','matcher_embedding','classifier_type','contentfilter')),
 candidate_source text NOT NULL DEFAULT '' CHECK (candidate_source IN ('','local','api')),
 task_key bytea CHECK (task_key IS NULL OR octet_length(task_key)=32),
 state text NOT NULL CHECK (state IN ('prepared','no_dispatch','admitted','intent','result','unknown')),
 lease_owner text CHECK (lease_owner IS NULL OR length(lease_owner)<=64),
 lease_generation bigint NOT NULL DEFAULT 0 CHECK (lease_generation>=0),
 lease_until timestamptz,
 reason text NOT NULL DEFAULT '' CHECK (length(reason)<=64),
 retry_after timestamptz,
 budget_scope text CHECK (budget_scope IN ('matcher','classifier_type','contentfilter','junkpurge')),
 reserved_day date,
 reserved_month date,
 response_sha256 bytea CHECK (response_sha256 IS NULL OR octet_length(response_sha256)=32),
 http_status integer CHECK (http_status BETWEEN 0 AND 599),
 error_class text CHECK (error_class IS NULL OR length(error_class)<=64),
 dispatch_intent_at timestamptz,
 result_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX llm_capture_dispatch_task_idx ON llm_capture_dispatch_attempts(task_key)
 WHERE task_key IS NOT NULL;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM llm_work_tasks) OR EXISTS (SELECT 1 FROM llm_capture_dispatch_attempts) THEN
    RAISE EXCEPTION 'retained optional model task history prevents destructive downgrade';
  END IF;
END $$;
DROP TABLE llm_capture_dispatch_attempts;
DROP TABLE llm_work_applications;
DROP TABLE llm_work_events;
DROP TABLE llm_work_tasks;
-- +goose StatementEnd
