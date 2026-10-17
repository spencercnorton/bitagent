-- +goose Up
-- A status window may only claim completeness after this conservative floor.
-- Earlier retention cannot be reconstructed from surviving rows. A fresh
-- complete sample is required after upgrading an existing catalogue.
ALTER TABLE processor_control ADD COLUMN attempt_history_floor timestamptz
 NOT NULL DEFAULT clock_timestamp();
CREATE INDEX processor_attempts_finished_window
 ON processor_attempts(processor_id,generation,finished_at DESC,job_id DESC,lease_generation DESC)
 WHERE finished_at IS NOT NULL;

-- +goose Down
DROP INDEX processor_attempts_finished_window;
ALTER TABLE processor_control DROP COLUMN attempt_history_floor;
