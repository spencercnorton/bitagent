-- +goose Up
-- +goose StatementBegin

-- torrent_contents_episodes_idx was a plain btree over the episodes jsonb —
-- vestigial upstream legacy. A btree on a whole jsonb value serves only
-- equality/ordering on the entire document, which no query path uses
-- (episode criteria use jsonb_exists / #> / @>, none btree-servable;
-- pg_stat_user_indexes showed idx_scan = 0 in prod). It DOES impose btree's
-- 2704-byte index-row ceiling on every write: since v0.46.0 the parser can
-- store full episode enumerations (large season/absolute-numbered packs), and
-- the first such value made every INSERT/UPDATE of that row fail with
-- SQLSTATE 54000 ("index row size exceeds btree maximum") — it aborted the
-- episodes-backfill and would equally abort a live classification of the
-- same name. Dropping it removes the write bomb, ~write amplification on
-- every tv-row update, and its share of index disk; nothing can regress
-- because nothing ever read it. If episode-shape queries ever need an index
-- it would be GIN, added CONCURRENTLY as its own migration.
drop index if exists torrent_contents_episodes_idx;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

create index torrent_contents_episodes_idx on torrent_contents using btree (episodes);

-- +goose StatementEnd
