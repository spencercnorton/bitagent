-- +goose NO TRANSACTION
-- +goose Up

-- An ordered matched prefix supports bounded representative validation.
-- This does not cache eligibility or change any catalogue row.
CREATE INDEX CONCURRENTLY IF NOT EXISTS torrent_contents_matched_seeder_page_idx
ON torrent_contents (COALESCE(seeders, -1) DESC, info_hash DESC)
INCLUDE (content_type, content_source, content_id)
WHERE content_id IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS torrent_contents_matched_seeder_page_idx;
