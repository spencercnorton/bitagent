-- +goose Up
-- +goose StatementBegin

-- Extends 00022 with the three tables it missed. Live diagnostics on
-- 2026-04-26 found:
--
--   torrent_contents — has NEVER been autovacuumed since boot. 42K dead
--                      vs 2.9M live = 1.4%, way below the default 0.20
--                      threshold. The 814 MB content_type_tsv GIN index
--                      will accumulate empty index entries forever
--                      without intervention.
--
--   bloom_filters    — 1 live row / 47 dead. The table is rotated by
--                      delete-then-reinsert on every regenerate cycle,
--                      so dead-tuple count grows linearly until autovacuum
--                      fires. With default thresholds and 1 live row,
--                      autovacuum never crosses 20% × 1 = 0.2 before
--                      another rotation adds 47 more dead.
--
--   queue_jobs       — processed jobs accumulate (13K observed) and the
--                      825 MB queue_payload index follows. Tighter
--                      autovacuum keeps the index lean between purge
--                      cycles (the purge worker itself is a separate
--                      follow-up per AGENTS/SPEC_db_hygiene_2026-04-26.md).
--
-- Tiny tables get an absolute threshold (autovacuum_vacuum_threshold)
-- instead of a scale factor, since 0.05 × 1 row never trips.

alter table torrent_contents set (
  autovacuum_vacuum_scale_factor = 0.05,
  autovacuum_analyze_scale_factor = 0.05
);

alter table bloom_filters set (
  autovacuum_vacuum_scale_factor = 0.0,
  autovacuum_vacuum_threshold = 10,
  autovacuum_analyze_threshold = 5
);

alter table queue_jobs set (
  autovacuum_vacuum_scale_factor = 0.05,
  autovacuum_analyze_scale_factor = 0.02
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table torrent_contents reset (
  autovacuum_vacuum_scale_factor,
  autovacuum_analyze_scale_factor
);

alter table bloom_filters reset (
  autovacuum_vacuum_scale_factor,
  autovacuum_vacuum_threshold,
  autovacuum_analyze_threshold
);

alter table queue_jobs reset (
  autovacuum_vacuum_scale_factor,
  autovacuum_analyze_scale_factor
);

-- +goose StatementEnd
