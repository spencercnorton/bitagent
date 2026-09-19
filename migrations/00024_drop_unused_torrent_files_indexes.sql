-- +goose Up
-- +goose StatementBegin

-- Drop two indexes on torrent_files that have NEVER been used by the
-- query planner. Live audit on the reference deployment 2026-04-26:
--
--   torrent_files_size_idx       (762 MB, idx_scan = 0 since stats_reset = NULL)
--   torrent_files_extension_idx  (464 MB, idx_scan = 0 since stats_reset = NULL)
--
-- pg_stat_database.stats_reset was NULL meaning these counters cover the
-- entire lifetime of the database since boot. 0 scans means the planner
-- has never picked these indexes for any query the bitagent binary or
-- the torznab adapter has issued. Net reclaim: ~1.2 GB.
--
-- The corresponding columns (torrent_files.size, torrent_files.extension)
-- are still indexed via the primary key + the unique constraint on
-- (info_hash, index) — neither is ever a top-level filter without an
-- info_hash join, and the planner already uses the PK for those joins.
--
-- Reversibility: CONCURRENTLY-rebuild via 00024_down restores them in a
-- maintenance window if a future query needs them. The index definitions
-- are inlined in the Down block so a rollback is mechanical.
--
-- IF CONCURRENTLY: not used here because goose Up runs in a transaction
-- and DROP INDEX CONCURRENTLY isn't transactional. The drop holds an
-- ACCESS EXCLUSIVE lock for ~50ms each (just metadata; the data files
-- are already on disk). For 1.2 GB of metadata, the unlink is async via
-- background writer. Acceptable for the qbt-vpn stack.

drop index if exists torrent_files_size_idx;
drop index if exists torrent_files_extension_idx;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

create index torrent_files_size_idx on torrent_files using btree (size);
create index torrent_files_extension_idx on torrent_files using btree (extension);

-- +goose StatementEnd
