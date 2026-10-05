-- +goose Up
-- +goose StatementBegin

-- Embedding inputs and responses are a distinct task. Their cosine shortlist
-- cannot impersonate a chat rerank response or authorize a match decision.
ALTER TABLE llm_evaluation_captures
  DROP CONSTRAINT llm_evaluation_captures_task_check,
  ADD CONSTRAINT llm_evaluation_captures_task_check
    CHECK (task IN ('matcher_extract', 'matcher_rerank', 'matcher_embedding', 'contentfilter', 'junkpurge', 'classifier_type'));

-- The original unnamed table constraint receives a generated name. Locate
-- that task/source binding by its expression rather than guessing its name.
DO $$
DECLARE binding text;
BEGIN
  FOR binding IN SELECT conname FROM pg_constraint
    WHERE conrelid = 'llm_evaluation_captures'::regclass AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%candidate_source IS NOT NULL%'
      AND pg_get_constraintdef(oid) LIKE '%matcher_rerank%'
  LOOP
    EXECUTE format('ALTER TABLE llm_evaluation_captures DROP CONSTRAINT %I', binding);
  END LOOP;
END $$;

ALTER TABLE llm_evaluation_captures
  ADD CONSTRAINT llm_evaluation_captures_candidate_task_check CHECK (
    (task IN ('matcher_rerank', 'matcher_embedding') AND candidate_source IS NOT NULL)
    OR (task NOT IN ('matcher_rerank', 'matcher_embedding') AND candidate_source IS NULL)
  );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Rollback fails closed while embedding captures remain. It never deletes
-- immutable request/response evidence to fit an older task contract.
ALTER TABLE llm_evaluation_captures
  DROP CONSTRAINT llm_evaluation_captures_task_check,
  ADD CONSTRAINT llm_evaluation_captures_task_check
    CHECK (task IN ('matcher_extract', 'matcher_rerank', 'contentfilter', 'junkpurge', 'classifier_type')),
  DROP CONSTRAINT llm_evaluation_captures_candidate_task_check,
  ADD CONSTRAINT llm_evaluation_captures_candidate_task_check CHECK (
    (task = 'matcher_rerank' AND candidate_source IS NOT NULL)
    OR (task <> 'matcher_rerank' AND candidate_source IS NULL)
  );

-- +goose StatementEnd
