package search

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/serving"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

type adultServingPostgresFixture struct {
	q      *dao.Query
	pool   *pgxpool.Pool
	hashes []protocol.ID
}

// This fixture is opt-in, uses invented data in its own schema, and makes no
// crawler, classifier, metadata-provider or model requests.
func newAdultServingPostgresFixture(t *testing.T) adultServingPostgresFixture {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("bitagent_adult_serving_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, cleanupErr := admin.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE")
		require.NoError(t, cleanupErr)
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	sqlDB := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
CREATE TABLE torrents (
  info_hash bytea PRIMARY KEY, name text NOT NULL, size bigint NOT NULL DEFAULT 4096,
  private boolean NOT NULL DEFAULT false, files_status text NOT NULL DEFAULT 'single',
  extension text DEFAULT 'mkv', files_count integer DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', name)) STORED
);
CREATE TABLE content (
  type text NOT NULL, source text NOT NULL, id text NOT NULL, title text NOT NULL,
  adult boolean, created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (type, source, id)
);
CREATE TABLE torrent_contents (
  id text PRIMARY KEY, info_hash bytea NOT NULL REFERENCES torrents(info_hash),
  content_type text, content_source text, content_id text, seeders bigint,
  languages jsonb, is_anime boolean NOT NULL DEFAULT false, size bigint NOT NULL DEFAULT 4096,
  published_at timestamptz NOT NULL DEFAULT now(),
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  search_string text NOT NULL DEFAULT 'synthetic fixture',
  tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple', search_string)) STORED
);
CREATE TABLE torrent_files (
  info_hash bytea NOT NULL REFERENCES torrents(info_hash), "index" integer NOT NULL DEFAULT 0,
  path text NOT NULL, extension text NOT NULL DEFAULT 'mkv', size bigint NOT NULL DEFAULT 4096,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (info_hash, path)
);
CREATE TABLE torrent_sources (
  key text PRIMARY KEY, name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE torrents_torrent_sources (
  info_hash bytea NOT NULL REFERENCES torrents(info_hash), source text NOT NULL,
  import_id text, seeders integer, leechers integer, published_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (source, info_hash)
);
CREATE TABLE junkpurge_quarantine (
  info_hash bytea PRIMARY KEY, torrent_name text NOT NULL, verdict text NOT NULL,
  confidence real NOT NULL, quarantined_at timestamptz NOT NULL DEFAULT now(),
  expired_at timestamptz, torrent_snapshot jsonb NOT NULL,
  files_snapshot jsonb, sources_snapshot jsonb
);
CREATE TABLE metadata_sources (key text PRIMARY KEY, name text NOT NULL);
CREATE TABLE content_attributes (
  content_type text, content_source text, content_id text, source text, key text, value text
);
CREATE TABLE content_collections (type text, source text, id text, name text);
CREATE TABLE content_collections_content (
  content_type text, content_source text, content_id text,
  content_collection_type text, content_collection_source text, content_collection_id text
);
CREATE TABLE torrent_hints (info_hash bytea PRIMARY KEY);
CREATE TABLE torrent_tags (info_hash bytea, name text);
INSERT INTO metadata_sources VALUES ('synthetic', 'Synthetic metadata');
INSERT INTO torrent_sources (key, name) VALUES ('synthetic', 'Synthetic source');
INSERT INTO content (type, source, id, title, adult) VALUES
  ('movie', 'synthetic', 'ordinary', 'Copper Acacia', false),
  ('movie', 'synthetic', 'adult', 'Synthetic Flagged Metadata', true),
  ('xxx', 'synthetic', 'typed', 'Synthetic Typed Metadata', false);
`)
	require.NoError(t, err)

	fixture := adultServingPostgresFixture{q: dao.Use(db), pool: pool}
	names := []string{
		"Synthetic.Typed.Release.mkv",
		"Synthetic.Flagged.Metadata.mkv",
		"Fixture.Siterip.mkv",
		"Fixture.Siterip.Attached.mkv",
		"Copper.Acacia.2031.mkv",
		// A strong keyword inside a longer word is not positive evidence.
		"Synthetic.SiteripReference.2031.mkv",
		// Unicode letters remain word characters; "adult" alone is weak.
		"ÉsiteripΩ.Adult.Education.2031.mkv",
		"Synthetic.Unclassified.2031.mkv",
		"Synthetic.Recrawled.Ordinary.2031.mkv",
	}
	for i, name := range names {
		var hash protocol.ID
		for j := range hash {
			hash[j] = byte(i + 1)
		}
		fixture.hashes = append(fixture.hashes, hash)
		_, err = pool.Exec(ctx, `INSERT INTO torrents (info_hash, name) VALUES ($1, $2)`, hash.Bytes(), name)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrent_files (info_hash, path) VALUES ($1, $2)`, hash.Bytes(), "synthetic/"+name)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrents_torrent_sources (info_hash, source, import_id, seeders)
VALUES ($1, 'synthetic', 'synthetic-import', $2)`, hash.Bytes(), i+1)
		require.NoError(t, err)
	}
	for _, row := range []struct {
		hashIndex   int
		contentType any
		contentID   any
		seeders     int
	}{
		{0, "xxx", "typed", 900},
		// Any adult projection hides the whole torrent, including its ordinary sibling.
		{0, "movie", "ordinary", 900},
		{1, "movie", "adult", 800},
		{2, nil, nil, 700},
		{3, "movie", "ordinary", 1000},
		{4, "movie", "ordinary", 10},
		{5, nil, nil, 9},
		{6, nil, nil, 8},
		// Retained quarantine plus a live row simulates reacquisition before processing.
		{8, "movie", "ordinary", 10000},
	} {
		var source any
		if row.contentID != nil {
			source = "synthetic"
		}
		_, err = pool.Exec(ctx, `INSERT INTO torrent_contents
  (id, info_hash, content_type, content_source, content_id, seeders)
VALUES ($1, $2, $3, $4, $5, $6)`,
			fmt.Sprintf("synthetic-%d-%v", row.hashIndex, row.contentType),
			fixture.hashes[row.hashIndex].Bytes(), row.contentType, source, row.contentID, row.seeders)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO junkpurge_quarantine
  (info_hash, torrent_name, verdict, confidence, expired_at, torrent_snapshot, files_snapshot, sources_snapshot)
SELECT t.info_hash, t.name, 'junk', 0.99, now() - interval '1 day', to_jsonb(t),
  (SELECT jsonb_agg(to_jsonb(f)) FROM torrent_files f WHERE f.info_hash = t.info_hash),
  (SELECT jsonb_agg(to_jsonb(s) || jsonb_build_object('source_metadata',
    jsonb_build_object('key', s.source, 'name', 'Synthetic origin')))
   FROM torrents_torrent_sources s WHERE s.info_hash = t.info_hash)
FROM torrents t WHERE t.info_hash = $1`, fixture.hashes[8].Bytes())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO torrent_tags (info_hash, name) VALUES
  ($1, 'synthetic-visible'), ($2, 'synthetic-adult'),
  ($3, 'synthetic-scene'), ($4, 'synthetic-quarantined')`,
		fixture.hashes[4].Bytes(), fixture.hashes[0].Bytes(), fixture.hashes[2].Bytes(), fixture.hashes[8].Bytes())
	require.NoError(t, err)
	return fixture
}

func (f adultServingPostgresFixture) serving(t *testing.T, excludeAdult bool) *search {
	t.Helper()
	policy, err := serving.NewPolicy(serving.Config{ExcludeAdult: excludeAdult}, []string{"siterip"}, []string{"mkv", "jpg"})
	require.NoError(t, err)
	return &search{q: f.q, adultPolicy: policy}
}

func adultServingExactOptions(table string) []query.Option {
	return []query.Option{
		query.WithTotalCount(true), query.WithAggregationBudget(0),
		query.OrderBy(query.OrderByColumn{OrderByColumn: clause.OrderByColumn{
			Column: clause.Column{Table: table, Name: "info_hash"},
		}}),
	}
}

type adultServingTableSnapshot struct {
	Count  int
	Digest string
}

func (f adultServingPostgresFixture) snapshot(t *testing.T) map[string]adultServingTableSnapshot {
	t.Helper()
	result := make(map[string]adultServingTableSnapshot)
	for _, table := range []string{
		"torrents", "torrent_contents", "torrent_files", "content",
		"torrent_sources", "torrents_torrent_sources", "torrent_tags", "junkpurge_quarantine",
	} {
		var snapshot adultServingTableSnapshot
		require.NoError(t, f.pool.QueryRow(context.Background(),
			`SELECT count(*), md5(coalesce(string_agg(to_jsonb(t)::text, E'\n' ORDER BY to_jsonb(t)::text), '')) FROM `+table+` t`).
			Scan(&snapshot.Count, &snapshot.Digest))
		result[table] = snapshot
	}
	return result
}

func adultServingContentHashes(items []TorrentContentResultItem) []protocol.ID {
	hashes := make([]protocol.ID, 0, len(items))
	for _, item := range items {
		hashes = append(hashes, item.InfoHash)
	}
	return hashes
}

func TestAdultServingPostgresFiltersBeforePaginationCountAndFacets(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	before := f.snapshot(t)
	s := f.serving(t, true)
	ctx := context.Background()
	for _, page := range []struct {
		offset uint
		want   []protocol.ID
		next   bool
	}{
		{0, f.hashes[4:5], true},
		{1, f.hashes[5:6], true},
		{2, f.hashes[6:7], false},
		{3, []protocol.ID{}, false},
	} {
		options := append(adultServingExactOptions(model.TableNameTorrentContent),
			query.Limit(1), query.Offset(page.offset), query.WithHasNextPage(true),
			query.WithFacet(TorrentContentTypeFacet(query.FacetIsAggregated())))
		result, err := s.TorrentContent(ctx, options...)
		require.NoError(t, err)
		require.EqualValues(t, 3, result.TotalCount)
		require.False(t, result.TotalCountIsEstimate)
		require.Equal(t, page.want, adultServingContentHashes(result.Items))
		require.Equal(t, page.next, result.HasNextPage)
		items := result.Aggregations[TorrentContentTypeFacetKey].Items
		require.EqualValues(t, 1, items["movie"].Count)
		require.EqualValues(t, 2, items["null"].Count)
		require.NotContains(t, items, "xxx")
	}
	grouped, err := s.TorrentContent(ctx, append(adultServingExactOptions(model.TableNameTorrentContent),
		TorrentContentGroupByContentOption(), TorrentContentDefaultHydrate(), query.Limit(1), query.WithHasNextPage(true))...)
	require.NoError(t, err)
	require.EqualValues(t, 1, grouped.TotalCount)
	require.Equal(t, f.hashes[4:5], adultServingContentHashes(grouped.Items), "hidden high-seeder rows must not select the representative")
	require.False(t, grouped.HasNextPage)
	require.Equal(t, "Copper Acacia", grouped.Items[0].Content.Title)
	require.Equal(t, f.hashes[4], grouped.Items[0].Torrent.InfoHash)
	metadata, err := s.Content(ctx, query.WithTotalCount(true), query.WithAggregationBudget(0))
	require.NoError(t, err)
	require.EqualValues(t, 1, metadata.TotalCount)
	require.Len(t, metadata.Items, 1)
	require.Equal(t, "ordinary", metadata.Items[0].ID)
	tags, err := s.TorrentSuggestTags(ctx, SuggestTagsQuery{})
	require.NoError(t, err)
	require.Len(t, tags.Suggestions, 1)
	require.Equal(t, "synthetic-visible", tags.Suggestions[0].Name)
	require.Equal(t, before, f.snapshot(t), "serving reads must preserve live rows, retained snapshots and source relationships")
}

func TestAdultServingPostgresExplicitHashAndTypeCannotBypass(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	s := f.serving(t, true)
	ctx := context.Background()
	for _, index := range []int{0, 1, 2, 3, 8} {
		hash := f.hashes[index]
		torrents, err := s.Torrents(ctx, append(adultServingExactOptions(model.TableNameTorrent), query.Where(TorrentInfoHashCriteria(hash)))...)
		require.NoError(t, err)
		require.Empty(t, torrents.Items)
		require.Zero(t, torrents.TotalCount)
		missing, err := s.TorrentsWithMissingInfoHashes(ctx, []protocol.ID{hash})
		require.NoError(t, err)
		require.Empty(t, missing.Torrents)
		require.Equal(t, []protocol.ID{hash}, missing.MissingInfoHashes)
		content, err := s.TorrentContent(ctx, append(adultServingExactOptions(model.TableNameTorrentContent), query.Where(TorrentContentInfoHashCriteria(hash)))...)
		require.NoError(t, err)
		require.Empty(t, content.Items)
		require.Zero(t, content.TotalCount)
		files, err := s.TorrentFiles(ctx, append(adultServingExactOptions(model.TableNameTorrentFile), query.Where(TorrentFileInfoHashCriteria(hash)))...)
		require.NoError(t, err)
		require.Empty(t, files.Items)
		require.Zero(t, files.TotalCount)
	}
	typed, err := s.TorrentContent(ctx,
		query.Where(TorrentContentTypeCriteria(model.ContentTypeXxx)),
		query.WithTotalCount(true), query.WithAggregationBudget(0),
		query.WithFacet(TorrentContentTypeFacet(query.FacetIsAggregated(), query.FacetHasFilter(query.FacetFilter{"xxx": {}}))))
	require.NoError(t, err)
	require.Empty(t, typed.Items)
	require.Zero(t, typed.TotalCount)
	require.Zero(t, typed.Aggregations[TorrentContentTypeFacetKey].Items["xxx"].Count)
	for _, ref := range []model.ContentRef{
		{Type: model.ContentTypeMovie, Source: "synthetic", ID: "adult"},
		{Type: model.ContentTypeXxx, Source: "synthetic", ID: "typed"},
	} {
		metadata, metadataErr := s.Content(ctx, query.Where(ContentCanonicalIdentifierCriteria(ref)),
			query.WithTotalCount(true), query.WithAggregationBudget(0))
		require.NoError(t, metadataErr)
		require.Empty(t, metadata.Items)
		require.Zero(t, metadata.TotalCount)
	}
}

func TestAdultServingPostgresExcludedFacetPreservesSelfAggregation(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	for _, grouped := range []bool{false, true} {
		for _, filter := range []query.FacetFilter{{"xxx": {}}, {"xxx": {}, "movie": {}}} {
			options := append(adultServingExactOptions(model.TableNameTorrentContent),
				query.WithFacet(TorrentContentTypeFacet(query.FacetIsAggregated(), query.FacetHasFilter(filter))))
			if grouped {
				options = append(options, TorrentContentGroupByContentOption())
			}
			result, err := f.serving(t, true).TorrentContent(context.Background(), options...)
			require.NoError(t, err)
			want := 0
			if filter.HasKey("movie") {
				want = 1
			}
			require.EqualValues(t, want, result.TotalCount)
			require.Len(t, result.Items, want)
			aggs := result.Aggregations[TorrentContentTypeFacetKey].Items
			require.Zero(t, aggs["xxx"].Count)
			require.False(t, aggs["xxx"].IsEstimate)
			require.EqualValues(t, 1, aggs["movie"].Count, "OR self-aggregation retains ordinary values")
			if !grouped {
				require.EqualValues(t, 2, aggs["null"].Count)
			}
		}
	}
	off, err := f.serving(t, false).TorrentContent(context.Background(),
		query.WithTotalCount(true), query.WithAggregationBudget(0),
		query.WithFacet(TorrentContentTypeFacet(query.FacetHasFilter(query.FacetFilter{"xxx": {}}))))
	require.NoError(t, err)
	require.Positive(t, off.TotalCount, "optimization is bound to active adult exclusion")
	require.NotEmpty(t, off.Items)
}

func TestAdultServingPostgresFlagOffRestoresAdultVisibilityWithoutMutation(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	before := f.snapshot(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		search      *search
		torrentRows uint
		contentRows uint
	}{
		{"raw internal", &search{q: f.q}, 9, 9},
		{"adult flag off", f.serving(t, false), 8, 8},
		{"adult flag on", f.serving(t, true), 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			torrents, err := tc.search.Torrents(ctx, adultServingExactOptions(model.TableNameTorrent)...)
			require.NoError(t, err)
			require.Equal(t, tc.torrentRows, torrents.TotalCount)
			require.Len(t, torrents.Items, int(tc.torrentRows))
			content, err := tc.search.TorrentContent(ctx, adultServingExactOptions(model.TableNameTorrentContent)...)
			require.NoError(t, err)
			require.Equal(t, tc.contentRows, content.TotalCount)
			require.Len(t, content.Items, int(tc.contentRows))
			files, err := tc.search.TorrentFiles(ctx, adultServingExactOptions(model.TableNameTorrentFile)...)
			require.NoError(t, err)
			require.Equal(t, tc.torrentRows, files.TotalCount)
			require.Len(t, files.Items, int(tc.torrentRows))
		})
	}
	// Turning the flag off restores the exact adult rows and original metadata;
	// retained quarantine remains an independent serving constraint.
	visible, err := f.serving(t, false).Torrents(ctx, adultServingExactOptions(model.TableNameTorrent)...)
	require.NoError(t, err)
	for i, item := range visible.Items {
		require.Equal(t, f.hashes[i], item.InfoHash)
	}
	metadata, err := f.serving(t, false).Content(ctx, query.WithTotalCount(true), query.WithAggregationBudget(0))
	require.NoError(t, err)
	require.EqualValues(t, 3, metadata.TotalCount)
	require.Len(t, metadata.Items, 3)
	grouped, err := f.serving(t, false).TorrentContent(ctx, TorrentContentGroupByContentOption(),
		query.WithTotalCount(true), query.WithAggregationBudget(0))
	require.NoError(t, err)
	require.EqualValues(t, 3, grouped.TotalCount)
	require.ElementsMatch(t, []protocol.ID{f.hashes[0], f.hashes[1], f.hashes[3]}, adultServingContentHashes(grouped.Items))
	require.Equal(t, before, f.snapshot(t), "flag changes and serving reads cannot rewrite the catalogue")
}

func TestAdultServingPostgresQuarantineRequiresBoundCompleteSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		update string
		hidden bool
	}{
		{"source complete expired", "sources_snapshot='[]'::jsonb", true},
		{"legacy sources absent", "sources_snapshot=NULL", false},
		{"sources object", "sources_snapshot='{}'::jsonb", false},
		{"raw snapshot array", "torrent_snapshot='[]'::jsonb", false},
		{"raw hash absent", "torrent_snapshot=torrent_snapshot-'info_hash'", false},
		{"raw hash mismatch", "torrent_snapshot=jsonb_set(torrent_snapshot,'{info_hash}',to_jsonb('synthetic-other'::text))", false},
		{"raw name mismatch", "torrent_snapshot=jsonb_set(torrent_snapshot,'{name}',to_jsonb('SyntheticOtherName'::text))", false},
		{"private snapshot", "torrent_snapshot=jsonb_set(torrent_snapshot,'{private}','true'::jsonb)", false},
		{"files object", "files_snapshot='{}'::jsonb", false},
		{"file hash absent", "files_snapshot='[{}]'::jsonb", false},
		{"source hash absent", "sources_snapshot='[{}]'::jsonb", false},
		{"source registry missing", "sources_snapshot=jsonb_set(sources_snapshot,'{0}',(sources_snapshot->0)-'source_metadata')", false},
		{"source registry mismatch", "sources_snapshot=jsonb_set(sources_snapshot,'{0,source_metadata,key}',to_jsonb('OtherSource'::text))", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdultServingPostgresFixture(t)
			ctx := context.Background()
			hash := f.hashes[8]
			_, err := f.pool.Exec(ctx, "UPDATE junkpurge_quarantine SET "+tc.update+" WHERE info_hash=$1", hash.Bytes())
			require.NoError(t, err)
			before := f.snapshot(t)
			for _, adult := range []bool{false, true} {
				s := f.serving(t, adult)
				got, err := s.Torrents(ctx, query.Where(TorrentInfoHashCriteria(hash)), query.WithTotalCount(true), query.WithAggregationBudget(0))
				require.NoError(t, err)
				want := 1
				if tc.hidden {
					want = 0
				}
				require.Len(t, got.Items, want)
				require.EqualValues(t, want, got.TotalCount)
				content, err := s.TorrentContent(ctx, query.Where(TorrentContentInfoHashCriteria(hash)), query.WithTotalCount(true), query.WithAggregationBudget(0))
				require.NoError(t, err)
				require.Len(t, content.Items, want)
				files, err := s.TorrentFiles(ctx, query.Where(TorrentFileInfoHashCriteria(hash)), query.WithTotalCount(true), query.WithAggregationBudget(0))
				require.NoError(t, err)
				require.Len(t, files.Items, want)
			}
			require.Equal(t, before, f.snapshot(t), "qualification reads do not rewrite legacy or corrupted evidence")
		})
	}
}

func TestAdultServingPostgresRetainedQuarantineSurvivesRecrawlUntilMarkerRemoval(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	before := f.snapshot(t)
	ctx := context.Background()
	hash := f.hashes[8]
	for _, excludeAdult := range []bool{false, true} {
		s := f.serving(t, excludeAdult)
		torrents, err := s.Torrents(ctx, query.Where(TorrentInfoHashCriteria(hash)), query.WithTotalCount(true), query.WithAggregationBudget(0))
		require.NoError(t, err)
		require.Empty(t, torrents.Items)
		require.Zero(t, torrents.TotalCount)
		files, err := s.TorrentFiles(ctx, query.Where(TorrentFileInfoHashCriteria(hash)))
		require.NoError(t, err)
		require.Empty(t, files.Items)
		tags, err := s.TorrentSuggestTags(ctx, SuggestTagsQuery{Prefix: "synthetic-quarantined"})
		require.NoError(t, err)
		require.Empty(t, tags.Suggestions)
		grouped, err := s.TorrentContent(ctx, TorrentContentGroupByContentOption(),
			query.WithTotalCount(true), query.WithAggregationBudget(0),
			query.WithFacet(TorrentContentTypeFacet(query.FacetIsAggregated())))
		require.NoError(t, err)
		require.Len(t, grouped.Items, int(grouped.TotalCount), "grouped rows and exact count must agree")
		for _, item := range grouped.Items {
			require.NotEqual(t, hash, item.InfoHash, "retained quarantine must not win an ordinary content group")
		}
		if excludeAdult {
			require.EqualValues(t, 1, grouped.TotalCount)
			require.EqualValues(t, 1, grouped.Aggregations[TorrentContentTypeFacetKey].Items["movie"].Count)
		} else {
			require.EqualValues(t, 3, grouped.TotalCount)
			require.EqualValues(t, 4, grouped.Aggregations[TorrentContentTypeFacetKey].Items["movie"].Count)
		}
	}
	require.Equal(t, before, f.snapshot(t), "retained tombstone, torrent/file snapshots and source payload survive serving")
	// Existing restore removes this marker. Simulate only that visibility
	// transition here; the restore package separately tests its full transaction.
	_, err := f.pool.Exec(ctx, "DELETE FROM junkpurge_quarantine WHERE info_hash=$1", hash.Bytes())
	require.NoError(t, err)
	for _, excludeAdult := range []bool{false, true} {
		s := f.serving(t, excludeAdult)
		torrents, searchErr := s.Torrents(ctx, query.Where(TorrentInfoHashCriteria(hash)))
		require.NoError(t, searchErr)
		require.Len(t, torrents.Items, 1)
		require.Equal(t, hash, torrents.Items[0].InfoHash)
		files, filesErr := s.TorrentFiles(ctx, query.Where(TorrentFileInfoHashCriteria(hash)))
		require.NoError(t, filesErr)
		require.Len(t, files.Items, 1)
		tags, tagsErr := s.TorrentSuggestTags(ctx, SuggestTagsQuery{Prefix: "synthetic-quarantined"})
		require.NoError(t, tagsErr)
		require.Len(t, tags.Suggestions, 1)
	}
	after := f.snapshot(t)
	delete(before, "junkpurge_quarantine")
	delete(after, "junkpurge_quarantine")
	require.Equal(t, before, after, "marker removal must not alter live rows or source relationships")
}
