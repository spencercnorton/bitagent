-- +goose Up
-- +goose StatementBegin

-- grab_outcome_priors stores per-feature Beta(alpha, beta) success
-- priors derived from observed *arr grab outcomes. The priors module
-- ingests Sonarr/Radarr Grab + Download/DownloadFolderImported webhook
-- events: a Download is a success (+alpha for every feature extracted
-- from the release title and source list); a Grab that does not see
-- a matching Download within ResolutionWindow is a failure (+beta).
--
-- key_type ∈ {source, release_group, quality_tag, codec, container,
--             extension, resolution, language}. Keep the set small —
-- we want every observation to update several keys to keep posteriors
-- moving even on cold starts.
--
-- The Torznab adapter consults this table to re-rank search results
-- before returning them to the *arr that asked, biasing toward feature
-- combinations with strong historical success priors.
create table grab_outcome_priors
(
  key_type   text        not null,
  key_value  text        not null,
  alpha      bigint      not null default 1,
  beta       bigint      not null default 1,
  updated_at timestamptz not null default now(),
  primary key (key_type, key_value)
);

create index grab_outcome_priors_updated_idx
  on grab_outcome_priors (updated_at desc);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists grab_outcome_priors;

-- +goose StatementEnd
