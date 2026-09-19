-- +goose Up
-- A process may exit after request capture but before its immutable provider
-- result is stored. Content-filter replay completes that at-most-once request
-- with an explicit local terminal state rather than dispatching a second paid
-- call or leaving an ambiguous capture forever.
ALTER TABLE llm_evaluation_capture_results
  DROP CONSTRAINT llm_evaluation_capture_results_error_class_check;

ALTER TABLE llm_evaluation_capture_results
  ADD CONSTRAINT llm_evaluation_capture_results_error_class_check
  CHECK (error_class IN
    ('none', 'transport', 'read', 'http_status', 'envelope', 'empty_choices',
     'audit_incomplete'));

-- +goose Down
-- Preserve rollback compatibility without deleting retained evidence. Older
-- binaries understand `transport`; the response body still identifies the
-- local audit recovery state.
UPDATE llm_evaluation_capture_results
SET error_class = 'transport'
WHERE error_class = 'audit_incomplete';

ALTER TABLE llm_evaluation_capture_results
  DROP CONSTRAINT llm_evaluation_capture_results_error_class_check;

ALTER TABLE llm_evaluation_capture_results
  ADD CONSTRAINT llm_evaluation_capture_results_error_class_check
  CHECK (error_class IN
    ('none', 'transport', 'read', 'http_status', 'envelope', 'empty_choices'));
