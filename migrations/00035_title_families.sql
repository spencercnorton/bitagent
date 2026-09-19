-- +goose Up
-- +goose StatementBegin

-- Title-family registry (family-disambiguation subsystem, work item F1).
--
-- A "family" is a set of mirror content entities whose titles are mutually
-- confusable — exact normalized-title collisions (remakes, regional versions,
-- live-action adaptations), brand-prefix extensions ("Dexter" → "Dexter New
-- Blood"), and official-alias collisions (FMA 2003 ↔ Brotherhood). Inside a
-- family, title similarity alone can never distinguish members; membership
-- must be queryable so a later decision policy (F6, flag-gated) can demand
-- positive evidence or abstain instead of guessing.
--
-- These tables are a PURE DERIVED CACHE built by `bitagent family-rebuild`
-- from content + content_attributes(alt_title:*) + anime_titles. The builder
-- rebuilds wholesale (TRUNCATE + COPY in one transaction, like anime_titles);
-- dropping or truncating them is always safe. Nothing reads them yet — F1 is
-- inert infrastructure; enforcement (F6/F7) is separately gated.
--
-- family ids are NOT stable across rebuilds. Anything durable must reference
-- members by (content_type, content_source, content_id), never by family_id.

create table title_families
(
  id                bigint      primary key,        -- builder-assigned within one build
  family_key        text        not null,           -- root brand key, or 'native:<key>' for CJK re-keys
  induction_version integer     not null,           -- monotonic build counter
  member_count      integer     not null,
  generic_root      boolean     not null default false, -- root is a generic word: prefix edges were suppressed
  built_at          timestamptz not null default now(),
  unique (family_key)
);

create table title_family_members
(
  family_id      bigint  not null references title_families (id) on delete cascade,
  content_type   text    not null,
  content_source text    not null,
  content_id     text    not null,
  member_key     text    not null,                  -- the member's own normalized title key
  release_year   integer,
  primary key (family_id, content_type, content_source, content_id)
);

-- Reverse lookup: is this entity family-resident, and in which family?
create index title_family_members_entity_idx
  on title_family_members (content_type, content_source, content_id);

-- Per-member distinguishing tokens induced at build time (tokens present in a
-- member's title/aliases but absent from the family's shared brand tokens).
-- The (family_id, token) primary key IS the disambiguation guarantee: a token
-- claimed by two members of one family is ambiguous and dropped at build time,
-- so a token hit always selects exactly one member.
create table title_family_tokens
(
  family_id      bigint not null references title_families (id) on delete cascade,
  token          text   not null,                   -- normalized token or ordered bigram ('new blood')
  content_type   text   not null,
  content_source text   not null,
  content_id     text   not null,
  source         text   not null,                   -- title | alias | year
  primary key (family_id, token)
);

-- Every member key + every alias key that unambiguously identifies one family.
-- lookup_key alone is the primary key: a key that would map to two families is
-- dropped at build time (never persisted), so a lookup never has to guess.
create table title_family_lookup
(
  lookup_key text   primary key,
  family_id  bigint not null references title_families (id) on delete cascade
);

create index title_family_lookup_family_idx on title_family_lookup (family_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists title_family_lookup;
drop table if exists title_family_tokens;
drop table if exists title_family_members;
drop table if exists title_families;

-- +goose StatementEnd
