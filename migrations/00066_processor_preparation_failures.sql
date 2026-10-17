-- +goose Up
-- Local preparation failures never count as dispatched processor attempts.
ALTER TABLE processor_jobs ADD COLUMN preparation_failures integer NOT NULL DEFAULT 0
 CHECK(preparation_failures >= 0);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM processor_jobs WHERE preparation_failures > 0) THEN
  RAISE EXCEPTION 'preparation failure evidence prevents destructive downgrade';
 END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE processor_jobs DROP COLUMN preparation_failures;
