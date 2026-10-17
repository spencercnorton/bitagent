-- +goose Up
ALTER TABLE processor_registry
 ADD COLUMN driver text NOT NULL DEFAULT 'http'
  CHECK(driver IN('http','builtin/filter-list','builtin/regex-weights','builtin/tracker-liveness','builtin/llm')),
 ADD COLUMN selector jsonb NOT NULL DEFAULT '{}'::jsonb
  CHECK(jsonb_typeof(selector)='object' AND octet_length(selector::text)<=16384),
 ADD COLUMN learning jsonb NOT NULL DEFAULT '{"enabled":false}'::jsonb
  CHECK(jsonb_typeof(learning)='object' AND octet_length(learning::text)<=16384),
 ADD COLUMN learned_state jsonb NOT NULL DEFAULT '{}'::jsonb
  CHECK(jsonb_typeof(learned_state)='object' AND octet_length(learned_state::text)<=262144);

CREATE TABLE processor_feedback_events (
 seq bigserial PRIMARY KEY,
 event_id text NOT NULL UNIQUE CHECK(event_id ~ '^[0-9a-f]{64}$'),
 processor_id text NOT NULL,
 execution_generation bigint NOT NULL,
 job_id text NOT NULL DEFAULT '',
 info_hash bytea NOT NULL CHECK(octet_length(info_hash)=20),
 tier text NOT NULL CHECK(tier IN('operational_receipt','teacher_consensus','verified_import','human_gold')),
 input jsonb,
 prediction jsonb,
 trace jsonb,
 ground_truth jsonb NOT NULL CHECK(jsonb_typeof(ground_truth)='object'),
 delivery_state text NOT NULL DEFAULT 'pending' CHECK(delivery_state IN('pending','delivered','pulled','failed','not_applicable')),
 delivery_attempts integer NOT NULL DEFAULT 0 CHECK(delivery_attempts >= 0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 delivered_at timestamptz
);
CREATE INDEX processor_feedback_events_processor_seq
 ON processor_feedback_events(processor_id, seq ASC);
CREATE INDEX processor_feedback_events_info_hash
 ON processor_feedback_events(info_hash, created_at DESC);
CREATE INDEX processor_feedback_events_pending_push
 ON processor_feedback_events(processor_id, seq ASC)
 WHERE delivery_state = 'pending';

CREATE TABLE processor_feedback_cursors (
 processor_id text NOT NULL,
 consumer_id text NOT NULL DEFAULT 'default',
 last_seq bigint NOT NULL DEFAULT 0,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(processor_id, consumer_id)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM processor_feedback_events) THEN
  RAISE EXCEPTION 'processor feedback evidence prevents destructive downgrade';
 END IF;
END $$;
-- +goose StatementEnd
DROP TABLE processor_feedback_cursors;
DROP TABLE processor_feedback_events;
ALTER TABLE processor_registry
 DROP COLUMN learned_state,
 DROP COLUMN learning,
 DROP COLUMN selector,
 DROP COLUMN driver;
