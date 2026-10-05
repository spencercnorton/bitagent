-- +goose Up
-- +goose StatementBegin

-- Retain source relationships and their registry metadata before torrent
-- deletion cascades. NULL identifies a legacy snapshot, which remains usable.
ALTER TABLE junkpurge_quarantine ADD COLUMN sources_snapshot jsonb;

-- This records an explicit local restore, not a tracker/DHT observation.
INSERT INTO torrent_sources (key, name, created_at, updated_at)
VALUES ('quarantine_restore', 'Local quarantine restore', now(), now())
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Refuse to discard retained recovery metadata. A matching backup is needed
-- for a downgrade while any snapshots use this column.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM junkpurge_quarantine WHERE sources_snapshot IS NOT NULL) THEN
    RAISE EXCEPTION 'quarantine source snapshots must be retained';
  END IF;
END $$;
ALTER TABLE junkpurge_quarantine DROP COLUMN sources_snapshot;

-- Keep local restore provenance on already-restored torrents.

-- +goose StatementEnd
