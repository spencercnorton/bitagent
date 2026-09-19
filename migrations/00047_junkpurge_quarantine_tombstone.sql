-- +goose Up
-- +goose StatementBegin

-- Quarantine expiry becomes a tombstone instead of a destruction.
--
-- Before: expireQuarantine blacklisted the info_hash in torrent_liveness and
-- hard-DELETEd the quarantine row, throwing away torrent_snapshot /
-- files_snapshot. That made the review window the point of no return, and it
-- made the system's false-junk RATE a deletion authorisation: an error past
-- day 30 was unrecoverable, so the rate had to be bounded before purge could
-- ever be re-armed. It cannot be bounded -- the 0.1% target is the rule-of-three
-- floor of a planned 3,000-case holdout, and 80-94% of the candidate population
-- is invisible to every detector built (see docs/design/junkpurge-safety-program.md).
--
-- After: expiry sets expired_at and keeps the row. quarantineJunkTx has already
-- removed the torrent from `torrents`, so the torrent left search and Torznab at
-- QUARANTINE time -- expiry never did any of the operational work. RestoreQuarantined
-- keeps working on an expired row unchanged, so every expiry stays reversible.
--
-- This deliberately keeps BOTH owner outcomes for D4 open. Never-expire is now the
-- default behaviour. If the owner instead elects to re-arm a destructive purge, this
-- column is the marker a second horizon would key on -- a one-query addition, not a
-- redesign.
--
-- Cost: ~840 B/row of retained jsonb snapshot, ~1 GB/year at the corrected
-- steady-state candidate inflow of ~2,000-2,600/day.
alter table junkpurge_quarantine
  add column expired_at timestamptz;

-- Serves both the count and the newest-first page in ListQuarantine, which now
-- shows live rows only. Partial, so it does not grow with the tombstone archive.
create index junkpurge_quarantine_live_idx
  on junkpurge_quarantine (quarantined_at desc)
  where expired_at is null;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop index if exists junkpurge_quarantine_live_idx;
alter table junkpurge_quarantine
  drop column if exists expired_at;

-- +goose StatementEnd
