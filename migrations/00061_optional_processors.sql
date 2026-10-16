-- +goose Up
-- These tables intentionally have no FK into catalogue recovery's cascade set.
CREATE TABLE processor_control (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 epoch bigint NOT NULL DEFAULT 1,
 scan_after bytea NOT NULL DEFAULT ''::bytea,
 updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO processor_control(singleton) VALUES(true);
CREATE SEQUENCE processor_registry_generation;
CREATE TABLE processor_registry (
 id text PRIMARY KEY CHECK(length(id)<=96),
 generation bigint NOT NULL CHECK(generation>0),
 execution_generation bigint NOT NULL CHECK(execution_generation>0),
 endpoint text NOT NULL CHECK(length(endpoint)<=1024),
 auth_env text NOT NULL DEFAULT '' CHECK(length(auth_env)<=96),
 mode text NOT NULL CHECK(mode IN('shadow','apply')),
 state text NOT NULL CHECK(state IN('active','paused','draining')),
 priority integer NOT NULL CHECK(priority BETWEEN 0 AND 1000),
 manifest jsonb NOT NULL CHECK(octet_length(manifest::text)<=32768),
 configuration jsonb NOT NULL CHECK(octet_length(configuration::text)<=4096),
 observed jsonb,
 observed_at timestamptz,
 last_error text NOT NULL DEFAULT '' CHECK(length(last_error)<=128),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE processor_jobs (
 job_id text PRIMARY KEY CHECK(job_id ~ '^[a-f0-9]{64}$'),
 info_hash bytea NOT NULL CHECK(octet_length(info_hash)=20),
 input_revision text NOT NULL CHECK(input_revision ~ '^[a-f0-9]{64}$'),
 pipeline_revision text NOT NULL CHECK(pipeline_revision ~ '^[a-f0-9]{64}$'),
 epoch bigint NOT NULL,
 input jsonb NOT NULL CHECK(octet_length(input::text)<=131072),
 source jsonb NOT NULL CHECK(octet_length(source::text)<=262144),
 plan jsonb NOT NULL CHECK(octet_length(plan::text)<=524288),
 prepared_job jsonb CHECK(octet_length(prepared_job::text)<=262144),
 step integer NOT NULL DEFAULT 0,
 step_attempts integer NOT NULL DEFAULT 0,
 attempts integer NOT NULL DEFAULT 0,
 results jsonb NOT NULL DEFAULT '[]'::jsonb CHECK(octet_length(results::text)<=1048576),
 state text NOT NULL DEFAULT 'queued' CHECK(state IN('queued','leased','deferred','completed','failed','canceled','stale')),
 reason text NOT NULL DEFAULT '' CHECK(length(reason)<=128),
 outcome text NOT NULL DEFAULT '',
 lease_generation bigint NOT NULL DEFAULT 0,
 lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 available_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 CHECK((state='leased')=(lease_until IS NOT NULL))
);
CREATE INDEX processor_jobs_ready ON processor_jobs(state,available_at,created_at);
CREATE INDEX processor_jobs_source ON processor_jobs(info_hash,pipeline_revision);
CREATE TABLE processor_attempts (
 job_id text NOT NULL REFERENCES processor_jobs(job_id) ON DELETE CASCADE,
 lease_generation bigint NOT NULL,
 processor_id text NOT NULL,
 generation bigint NOT NULL,
 stage text NOT NULL,
 started_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 queue_wait_ms double precision NOT NULL,
 inference_ms double precision,
 outcome text NOT NULL DEFAULT '',
 reason text NOT NULL DEFAULT '',
 PRIMARY KEY(job_id,lease_generation)
);
-- One latest applied receipt per torrent; independent from transient job retention.
-- Snapshot/evidence histories already live in catalogue recovery/legacy tables.
CREATE TABLE processor_applications (
 info_hash bytea PRIMARY KEY CHECK(octet_length(info_hash)=20),
 job_id text NOT NULL,
 input_revision text NOT NULL,
 applied_revision text NOT NULL,
 pipeline_revision text NOT NULL,
 results jsonb NOT NULL CHECK(octet_length(results::text)<=1048576),
 applied_at timestamptz NOT NULL DEFAULT now()
);
-- Exclusions are restrictions only, never an eligibility grant. They survive
-- plugin removal/rollback until an explicit core/operator policy review.
CREATE TABLE processor_exclusions (
 info_hash bytea PRIMARY KEY CHECK(octet_length(info_hash)=20),
 job_id text NOT NULL,
 processor_id text NOT NULL,
 binding jsonb NOT NULL CHECK(octet_length(binding::text)<=4096),
 reason text NOT NULL CHECK(length(reason)<=128),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE processor_core_receipts (
 info_hash bytea PRIMARY KEY CHECK(octet_length(info_hash)=20),
 source_digest bytea NOT NULL CHECK(octet_length(source_digest)=32),
 content_id text NOT NULL,
 core_policy text NOT NULL,
 processing_outcome text NOT NULL CHECK(processing_outcome IN('completed','deferred','not_attempted')),
 outcome text NOT NULL CHECK(outcome IN('matched','completed_unmatched','deferred','not_attempted')),
 recorded_at timestamptz NOT NULL DEFAULT now()
);

-- Retain a complete before image of unfinished legacy work for rollback review.
CREATE TABLE processor_legacy_freeze (
 task_key bytea PRIMARY KEY,
 snapshot jsonb NOT NULL,
 frozen_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO processor_legacy_freeze(task_key,snapshot)
 SELECT task_key,to_jsonb(w) FROM llm_work_tasks w WHERE state IN('queued','deferred','leased');
INSERT INTO llm_work_events(task_key,state,reason)
 SELECT task_key,'held','embedded_runtime_retired' FROM processor_legacy_freeze;
UPDATE llm_work_tasks SET state='held',reason='embedded_runtime_retired',lease_owner=NULL,lease_until=NULL
 WHERE task_key IN(SELECT task_key FROM processor_legacy_freeze);
-- Hosted Batch runs, provider dispatch fences, captures, applications and
-- quarantine snapshots remain byte-for-byte intact. No network call is made.

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM processor_jobs) OR EXISTS(SELECT 1 FROM processor_applications)
 OR EXISTS(SELECT 1 FROM processor_core_receipts) OR EXISTS(SELECT 1 FROM processor_registry)
 OR EXISTS(SELECT 1 FROM processor_exclusions) OR EXISTS(SELECT 1 FROM processor_legacy_freeze) THEN
 RAISE EXCEPTION 'processor evidence prevents destructive downgrade; restore a matched backup';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE processor_core_receipts,processor_legacy_freeze,processor_exclusions,processor_applications,processor_attempts,processor_jobs,processor_registry,processor_control;

DROP SEQUENCE processor_registry_generation;
