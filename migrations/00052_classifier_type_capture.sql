-- +goose Up
-- Type-only fallback is a distinct audit task, not matcher or language-filter
-- evidence. Existing matcher captures and their immutable results are unchanged.
ALTER TABLE llm_evaluation_captures
  DROP CONSTRAINT llm_evaluation_captures_task_check,
  ADD CONSTRAINT llm_evaluation_captures_task_check
    CHECK (task IN ('matcher_extract', 'matcher_rerank', 'contentfilter', 'junkpurge', 'classifier_type'));

-- +goose Down
-- Deliberately fail closed if type captures remain: rollback must not silently
-- delete their request/result evidence to restore the older task contract.
ALTER TABLE llm_evaluation_captures
  DROP CONSTRAINT llm_evaluation_captures_task_check,
  ADD CONSTRAINT llm_evaluation_captures_task_check
    CHECK (task IN ('matcher_extract', 'matcher_rerank', 'contentfilter', 'junkpurge'));
