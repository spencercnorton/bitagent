-- +goose Up
-- +goose StatementBegin

-- Seed-history columns on the tracker ledger, maintained inline by the
-- worker's upsert on every scrape:
--   * prev_seeders     — the seeders value the PREVIOUS scrape recorded
--                        (NULL until a row has been scraped twice). With the
--                        current value this yields per-interval velocity —
--                        the "is this swarm draining or growing" signal the
--                        retention/verdict roadmap needs.
--   * peak_seeders     — highest authoritative count ever recorded. Replaces
--                        the accidental role the pre-v0.52.0 MAX bug gave
--                        fossil DHT peaks, but honestly labelled as history.
--   * last_positive_at — when the swarm last showed >0 seeders; the honest
--                        "how long dead" clock for demotion ladders (a
--                        known-zero row with last_positive_at 6 months back
--                        is a very different object from one that seeded
--                        yesterday).
-- Backfill: peak/last_positive seeded from the CURRENT reading where
-- positive — a single metadata-cheap UPDATE over ~2.1M ledger rows would be
-- minutes inside the goose startup transaction (same crash-loop hazard as
-- 00037), so history simply STARTS at the next scrape of each row: prev is
-- NULL by definition (no previous post-migration scrape), and peak/
-- last_positive accrue as the ~daily cycle touches every row. No CLI needed.
alter table torrent_tracker_seeds
  add column prev_seeders     integer,
  add column peak_seeders     integer,
  add column last_positive_at timestamptz;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table torrent_tracker_seeds
  drop column if exists prev_seeders,
  drop column if exists peak_seeders,
  drop column if exists last_positive_at;

-- +goose StatementEnd
