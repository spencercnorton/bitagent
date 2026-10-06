-- +goose Up
ALTER TABLE torrent_contents ADD COLUMN release_attributes jsonb;
ALTER TABLE torrent_contents ADD CONSTRAINT torrent_content_release_attributes_object
    CHECK (release_attributes IS NULL OR
        (jsonb_typeof(release_attributes) = 'object'
         AND coalesce(jsonb_typeof(release_attributes->'version'),'') = 'number'
         AND release_attributes->>'version' = '1'
         AND coalesce(release_attributes->>'parser','') = 'release-attributes-v1'
         AND coalesce(release_attributes->>'sourceNameSha256','') ~ '^[0-9a-f]{64}$'));

CREATE TABLE release_field_repair_journal (
 plan_digest bytea NOT NULL CHECK(octet_length(plan_digest)=32),
 target_id text NOT NULL,
 info_hash bytea NOT NULL CHECK(octet_length(info_hash)=20),
 source_name_sha256 text NOT NULL CHECK(source_name_sha256 ~ '^[0-9a-f]{64}$'),
 before_fields jsonb NOT NULL CHECK(jsonb_typeof(before_fields)='object'),
 after_fields jsonb NOT NULL CHECK(jsonb_typeof(after_fields)='object'),
 application_before jsonb NOT NULL CHECK(jsonb_typeof(application_before)='object'),
 application_after jsonb NOT NULL CHECK(jsonb_typeof(application_after)='object'),
 applied_updated_at timestamptz NOT NULL,
 applied_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 rolled_back_at timestamptz,
 PRIMARY KEY(plan_digest,target_id)
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM torrent_contents WHERE release_attributes IS NOT NULL) OR EXISTS(SELECT 1 FROM release_field_repair_journal) THEN
        RAISE EXCEPTION 'cannot discard retained release attributes or repair history; restore a matching backup before downgrade';
    END IF;
END $$;
DROP TABLE release_field_repair_journal;
ALTER TABLE torrent_contents DROP COLUMN release_attributes;
-- +goose StatementEnd
