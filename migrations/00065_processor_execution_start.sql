-- +goose Up
-- Existing execution generations have no provable creation timestamp. Leave
-- them unknown; never manufacture an empty startup population during upgrade.
ALTER TABLE processor_registry ADD COLUMN execution_started_at timestamptz;

-- +goose Down
ALTER TABLE processor_registry DROP COLUMN execution_started_at;
