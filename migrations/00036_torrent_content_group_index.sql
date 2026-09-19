-- +goose NO TRANSACTION
-- +goose Up
--
-- Server-side grouped-title pagination (groupByContent) support.
--
-- The public library (bitagent-ui) groups torrent_contents rows into one card
-- per title (content) client-side, capped at a 500-row fetch. The new
-- `groupByContent` search flag moves that grouping server-side via a
-- DISTINCT-ON-per-(content_type, content_source, content_id) derived table
-- whose representative is the highest-seeded torrent. This index is what makes
-- that plan index-ordered instead of a full sort of the matched corpus.
--
-- BENCHMARK (live Galactic-Torrent PG16, 3,157,643 torrent_contents rows;
-- 1,492,414 matched / 108,818 distinct matched title-groups):
--   * WITHOUT this index the DISTINCT-ON representative selection sorts all
--     ~1.49M matched rows (planner cost ~600k) on every grouped browse.
--   * WITH this index the planner feeds the Unique node from an ordered
--     Index Scan (no sort of the corpus).
-- The column order and per-column direction below MUST mirror the inner
-- representative ORDER BY exactly, or Postgres cannot use the index for the
-- ordered read and silently reverts to the full-sort plan:
--     ORDER BY content_type, content_source, content_id,
--              COALESCE(seeders, -1) DESC, info_hash
--   - content_type/content_source/content_id: ASC (btree default). The CHECK
--     constraints on torrent_contents guarantee these are non-null whenever
--     content_id is non-null, so NULLS ordering is moot within the matched set.
--   - COALESCE(seeders, -1) DESC: matches the existing seeders comparator
--     (order_torrent_content.go) so NULL seeders sort last (as -1) and the
--     representative is the genuinely highest-seeded torrent.
--   - info_hash: the final, deterministic tiebreak. Within a content group the
--     only per-torrent uniqueness is info_hash (UNIQUE(info_hash, content_type,
--     content_source, content_id)); without it the representative among
--     seeder-ties (240,859 rows currently have NULL seeders, all colliding at
--     -1) is arbitrary and offset pagination can duplicate/skip a title.
--
-- PARTIAL (WHERE content_id IS NOT NULL): grouped mode is matched-only
-- (grouping by a content key is meaningless for the ~53% of the corpus with no
-- content), so the index only needs to cover the ~1.49M matched rows. This
-- halves its size and exactly matches the grouped query's own predicate.
--
-- CONCURRENTLY + NO TRANSACTION: this is the LIVE DHT-crawler's primary table
-- and it ingests continuously (autovacuum was tuned for exactly that churn in
-- 00022). A plain in-transaction CREATE INDEX takes ACCESS EXCLUSIVE for the
-- whole ~1.49M-row build and would stall crawler writes for minutes. goose Up
-- runs in a transaction by default (see 00024), so the NO TRANSACTION
-- annotation at the top of this file opts out so CONCURRENTLY can run.
-- (NB: never quote a goose annotation verbatim in a comment — the parser
-- treats any line containing the marker as an annotation.)

-- +goose StatementBegin
CREATE INDEX CONCURRENTLY IF NOT EXISTS torrent_contents_content_group_idx
  ON torrent_contents (
    content_type,
    content_source,
    content_id,
    (COALESCE(seeders, -1)) DESC,
    info_hash
  )
  WHERE content_id IS NOT NULL;
-- +goose StatementEnd

-- Multi-column n_distinct statistics for the grouping key. The grouped
-- totalCount is a budgeted_count() planner estimate (a grouped DISTINCT-ON over
-- ~1.49M rows always exceeds the default 5000 cost budget, so the count is
-- always the planner's Plan Rows estimate, never an exact scan). These extended
-- stats improve multi-column cardinality estimation for the grouping key, which
-- helps the planner choose sane grouped/faceted plans.
--
-- HONEST CAVEAT: they do NOT make totalCount accurate. (content_type,
-- content_source, content_id) is very high cardinality (~108k distinct tuples
-- in ~1.49M rows), and ANALYZE's sampled n_distinct materially underestimates
-- high-cardinality distinctness (measured ~11k vs ~108k actual at benchmark
-- time). Grouped totalCount is therefore a rough, typically-low estimate; it is
-- returned with totalCountIsEstimate=true and callers must page off hasNextPage,
-- never ceil(totalCount / limit). See the groupByContent schema docs.
-- +goose StatementBegin
CREATE STATISTICS IF NOT EXISTS torrent_contents_content_key_ndistinct (ndistinct)
  ON content_type, content_source, content_id
  FROM torrent_contents;
-- +goose StatementEnd

-- +goose StatementBegin
ANALYZE torrent_contents;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP STATISTICS IF EXISTS torrent_contents_content_key_ndistinct;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX CONCURRENTLY IF EXISTS torrent_contents_content_group_idx;
-- +goose StatementEnd
