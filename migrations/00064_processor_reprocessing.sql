-- +goose Up
-- Independent of catalogue/job retention: explicitly requested reprocessing
-- retains its original export and current before-images for audit and recovery.
CREATE TABLE processor_reprocess_requests (
 request_id text PRIMARY KEY CHECK(request_id ~ '^[0-9a-f]{64}$'),
 request_digest text NOT NULL,
 origin_job_id text NOT NULL,
 info_hash bytea NOT NULL CHECK(octet_length(info_hash)=20),
 expected_pipeline_revision text NOT NULL,
 reason text NOT NULL CHECK(length(reason)<=96),
 state text NOT NULL CHECK(state IN('waiting_core','ready','queued','settled','core_only','held','deferred','failed','stale','canceled')),
 state_reason text NOT NULL DEFAULT '',
 core_job_id text NOT NULL,
 core_receipt jsonb,
 result_job_id text,
 origin_export jsonb NOT NULL CHECK(octet_length(origin_export::text)<=5242880),
 before_snapshot jsonb NOT NULL CHECK(octet_length(before_snapshot::text)<=2097152),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE UNIQUE INDEX processor_reprocess_active ON processor_reprocess_requests(info_hash)
 WHERE state IN('waiting_core','ready','queued');
CREATE INDEX processor_reprocess_source ON processor_reprocess_requests(info_hash,created_at DESC,request_id DESC);
CREATE INDEX processor_reprocess_waiting ON processor_reprocess_requests(state,created_at);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM processor_reprocess_requests) THEN
  RAISE EXCEPTION 'reprocessing audit evidence prevents destructive downgrade';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE processor_reprocess_requests;
