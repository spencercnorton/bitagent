-- +goose Up
-- +goose StatementBegin

-- label_evidence is an append-only record of every category/media-type
-- signal observed from an external truth source (qBittorrent instances,
-- Sonarr/Radarr/Readarr/Lidarr APIs and webhooks). Evidence is never
-- authoritative by itself; the torrent_canonical_labels table derives
-- the winning label per infohash via the precedence rules documented
-- in ARCHITECTURE.md §1.
create table label_evidence
(
  id               bigserial   primary key,
  source           text        not null,
  source_kind      text        not null,
  source_instance  text        not null,
  source_object_id text        not null,
  download_id      text,
  info_hash        bytea,
  title            text,
  media_type       text,
  media_id         text,
  category         text,
  observed_at      timestamptz not null,
  strength         smallint    not null,
  raw_payload      jsonb,
  created_at       timestamptz not null default now()
);

-- Dedupe: the same (source instance, kind, object id) tuple must not
-- produce two evidence rows. A webhook retry or poll overlap will hit
-- this and be absorbed by ON CONFLICT DO NOTHING at the ingest layer.
create unique index label_evidence_dedupe
  on label_evidence (source, source_kind, source_instance, source_object_id);

create index label_evidence_info_hash_idx
  on label_evidence (info_hash) where info_hash is not null;

create index label_evidence_download_id_idx
  on label_evidence (download_id) where download_id is not null;

create index label_evidence_observed_at_idx
  on label_evidence (observed_at desc);

-- torrent_canonical_labels holds the current winning label per infohash
-- as derived from label_evidence. One row per infohash. Updated by the
-- resolver on every evidence insert; a new row wins only when its
-- strength exceeds the current winner, or ties on strength and is more
-- recent. Rows here preempt the classifier entirely.
create table torrent_canonical_labels
(
  info_hash         bytea       primary key,
  media_type        text,
  media_id          text,
  category          text,
  title             text,
  resolved_from     bigint      references label_evidence(id) on delete set null,
  resolved_source   text        not null,
  resolved_strength smallint    not null,
  resolved_at       timestamptz not null
);

create index torrent_canonical_labels_media_type_idx
  on torrent_canonical_labels (media_type);

create index torrent_canonical_labels_category_idx
  on torrent_canonical_labels (category);

create index torrent_canonical_labels_resolved_at_idx
  on torrent_canonical_labels (resolved_at desc);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists torrent_canonical_labels;
drop table if exists label_evidence;

-- +goose StatementEnd
