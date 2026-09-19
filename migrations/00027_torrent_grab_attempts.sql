-- +goose Up
-- +goose StatementBegin

-- torrent_grab_attempts tracks pending *arr Grab events whose
-- success or failure has not yet been resolved. A row is created on
-- every KindWebhookGrab observation (Sonarr/Radarr Connect → Webhook,
-- eventType=Grab) and resolved when the matching infohash sees a
-- KindWebhookImport event (success) or when the expirer worker
-- decides ResolutionWindow has elapsed (failure).
--
-- features is the frozen feature set extracted at grab time. Freezing
-- the features prevents the resolver from having to re-derive them
-- from a possibly-mutated catalog row days later, and keeps the
-- (key_type, key_value) updates idempotent on retried imports.
--
-- This table is intentionally lossy: rows are evicted as soon as they
-- are resolved (resolved_at set + outcome counted). The append-only
-- audit trail lives in label_evidence — torrent_grab_attempts is the
-- short-lived projection that drives prior updates.
create table torrent_grab_attempts
(
  id              bigserial   primary key,
  info_hash       bytea       not null,
  source          text        not null,
  source_instance text        not null,
  release_title   text        not null,
  features        jsonb       not null,
  grabbed_at      timestamptz not null default now(),
  resolved_at     timestamptz,
  outcome         text        check (outcome in ('success', 'failure'))
);

-- The expirer worker scans for pending grabs whose grabbed_at has
-- crossed the ResolutionWindow. A partial index keeps the scan cheap
-- regardless of how many resolved rows accumulate before the table
-- is purged.
create index torrent_grab_attempts_pending_idx
  on torrent_grab_attempts (grabbed_at)
  where resolved_at is null;

-- The import resolver looks up pending grabs by info_hash. Multiple
-- pending rows are possible (same hash grabbed by Sonarr AND Radarr
-- in error, or re-grabbed after a stall) — the resolver collapses
-- them all on a single import.
create index torrent_grab_attempts_pending_hash_idx
  on torrent_grab_attempts (info_hash)
  where resolved_at is null;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists torrent_grab_attempts;

-- +goose StatementEnd
