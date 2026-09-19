-- +goose Up
CREATE TABLE llm_evaluation_capture_results (
    capture_key bytea PRIMARY KEY
      REFERENCES llm_evaluation_captures(capture_key) ON DELETE CASCADE,
    response_body bytea NOT NULL CHECK (octet_length(response_body) <= 131072),
    response_sha256 bytea NOT NULL CHECK (octet_length(response_sha256) = 32),
    http_status integer NOT NULL CHECK (http_status BETWEEN 0 AND 599),
    error_class text NOT NULL CHECK (error_class IN
      ('none', 'transport', 'read', 'http_status', 'envelope', 'empty_choices')),
    observed_at timestamptz NOT NULL,
    decision jsonb CHECK (jsonb_typeof(decision) = 'object' AND octet_length(decision::text) <= 16384),
    decided_at timestamptz,
    CHECK ((decision IS NULL) = (decided_at IS NULL))
);

-- One immutable first HTTP completion per captured request, not every cache
-- hit or retry. Response rows inherit both expiry and row-cap eviction from
-- their parent capture. A final decision may be appended once, never replaced.
CREATE INDEX llm_evaluation_capture_results_observed_idx
    ON llm_evaluation_capture_results(observed_at, capture_key);

-- +goose Down
DROP TABLE llm_evaluation_capture_results;
