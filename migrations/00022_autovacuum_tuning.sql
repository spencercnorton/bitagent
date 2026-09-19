-- +goose Up
-- +goose StatementBegin

-- Tighten per-table autovacuum thresholds on the three high-churn tables.
--
-- Default `autovacuum_vacuum_scale_factor` is 0.20 — autovacuum waits until
-- 20% of a table's row count is dead tuples before reclaiming. On a DHT
-- crawler this is too slack: a few days of steady ingestion pushes
-- `torrent_files` past 15% dead ratio (observed 2026-04-24), and index
-- scans start to feel it. We drop the threshold table-by-table based on
-- observed churn profile, and tighten ANALYZE similarly so the planner's
-- row-count estimates don't drift away from reality between vacuums.
--
-- Values are intentionally conservative — autovacuum still runs in the
-- background on its own schedule, it just wakes up sooner. The
-- `bitmagnet_postgres_table_dead_tuples` / `..._autovacuum_last_age`
-- metrics will show the effect within ~24h of deploy.
--
-- Per-table rationale:
--   torrent_files                — highest-churn: every torrent metadata
--                                  fetch inserts many rows; updates rare.
--                                  Threshold: 2%.
--   torrents                     — primary metadata table, moderate churn
--                                  (updated on re-scrape). Threshold: 5%.
--   torrents_torrent_sources     — every DHT re-announce touches
--                                  updated_at. High update volume on a
--                                  stable row count. Threshold: 5%.

alter table torrent_files set (
  autovacuum_vacuum_scale_factor = 0.02,
  autovacuum_analyze_scale_factor = 0.02
);

alter table torrents set (
  autovacuum_vacuum_scale_factor = 0.05,
  autovacuum_analyze_scale_factor = 0.05
);

alter table torrents_torrent_sources set (
  autovacuum_vacuum_scale_factor = 0.05,
  autovacuum_analyze_scale_factor = 0.05
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table torrent_files reset (
  autovacuum_vacuum_scale_factor,
  autovacuum_analyze_scale_factor
);

alter table torrents reset (
  autovacuum_vacuum_scale_factor,
  autovacuum_analyze_scale_factor
);

alter table torrents_torrent_sources reset (
  autovacuum_vacuum_scale_factor,
  autovacuum_analyze_scale_factor
);

-- +goose StatementEnd
