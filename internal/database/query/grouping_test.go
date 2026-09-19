package query

import (
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func newDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()

	sqlDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}

	t.Cleanup(func() { _ = sqlDB.Close() })

	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}

	return gdb
}

// seedersDescOrder mirrors search.TorrentContentOrderBy seeders-descending: the
// COALESCE-wrapped raw column plus the info_hash tiebreak.
func seedersDescOrder() []OrderByColumn {
	return []OrderByColumn{
		{OrderByColumn: clause.OrderByColumn{
			Column: clause.Column{Name: "coalesce(torrent_contents.seeders, -1)", Raw: true},
			Desc:   true,
		}},
		{OrderByColumn: clause.OrderByColumn{
			Column: clause.Column{Table: "torrent_contents", Name: "info_hash"},
			Desc:   true,
		}},
	}
}

const (
	testGroupKeySQL   = "torrent_contents.content_type, torrent_contents.content_source, torrent_contents.content_id"
	testGroupOrderSQL = testGroupKeySQL + ", COALESCE(torrent_contents.seeders, -1) DESC, torrent_contents.info_hash"
)

func groupedBuilder() optionBuilder {
	b := newQueryContext(dbContext{tableName: "torrent_contents"}).
		GroupByDistinctOn(testGroupKeySQL, testGroupOrderSQL).
		OrderBy(seedersDescOrder()...).
		Limit(20).
		WithHasNextPage(true).
		Offset(40)

	return b.(optionBuilder)
}

func TestGroupedApplySelectEmitsDistinctOn(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	b := groupedBuilder()

	sql := gdb.ToSQL(func(tx *gorm.DB) *gorm.DB {
		tx = tx.Table("torrent_contents")
		if err := b.applySelect(tx.Statement.DB, true); err != nil {
			t.Fatalf("applySelect: %v", err)
		}

		return tx.Find(&map[string]any{})
	})

	t.Logf("grouped inner SELECT SQL:\n%s", sql)

	wantDistinctOn := "DISTINCT ON (" + testGroupKeySQL + ") torrent_contents.*"
	wants := []string{wantDistinctOn, "AS _order_0", "AS _order_1"}

	for _, w := range wants {
		if !strings.Contains(sql, w) {
			t.Errorf("grouped inner SELECT missing %q in:\n%s", w, sql)
		}
	}
}

// TestGroupedFullQuerySQL reproduces exactly what doItemsGrouped builds (inner
// DISTINCT-ON subquery + representative ORDER BY, wrapped in a derived table
// re-ordered/paginated by the outer) so the emitted SQL is asserted and logged
// for benchmarking. It uses the unfiltered matched-only browse (order by
// seeders), the worst-case shape.
func TestGroupedFullQuerySQL(t *testing.T) {
	t.Parallel()

	gdb := newDryRunDB(t)
	b := groupedBuilder()

	inner := gdb.ToSQL(func(tx *gorm.DB) *gorm.DB {
		tx = tx.Table("torrent_contents").Where("torrent_contents.content_id IS NOT NULL")
		if err := b.applySelect(tx.Statement.DB, true); err != nil {
			t.Fatalf("applySelect: %v", err)
		}

		return tx.Find(&map[string]any{})
	})
	inner += " ORDER BY " + b.grouping.innerOrderSQL

	outer := gdb.ToSQL(func(tx *gorm.DB) *gorm.DB {
		tx = tx.Table("(" + inner + ") AS grouped")
		if err := b.applyPost(tx.Statement.DB); err != nil {
			t.Fatalf("applyPost: %v", err)
		}

		return tx.Find(&map[string]any{})
	})

	t.Logf("grouped full query SQL:\n%s", outer)

	// Inner representative ordering: key-leading (DISTINCT ON requirement),
	// highest-seeded, deterministic info_hash tiebreak.
	wantInnerOrder := "ORDER BY " + testGroupOrderSQL

	wants := []string{
		// The inner derived table dedups via DISTINCT ON.
		"DISTINCT ON (" + testGroupKeySQL + ")",
		// Representative selection: key-leading, highest-seeded, info_hash tiebreak.
		wantInnerOrder,
		// Matched-only predicate flows into the inner.
		"torrent_contents.content_id IS NOT NULL",
		// Outer wraps the inner as a derived table...
		") AS grouped",
		// ...and re-orders representatives by the user's aliases.
		`ORDER BY "_order_0" DESC,"_order_1" DESC`,
		// hasNextPage: LIMIT is page size + 1 (21), applied on the OUTER.
		"LIMIT 21",
		// OFFSET is applied on the OUTER, not the inner representative selection.
		"OFFSET 40",
	}

	for _, w := range wants {
		if !strings.Contains(outer, w) {
			t.Errorf("grouped full query missing %q in:\n%s", w, outer)
		}
	}
}

// TestGroupedOptionSetsSpec confirms the query-level option threads a grouping
// spec onto the builder (and that a nil spec means the flat, ungrouped path).
func TestGroupedOptionSetsSpec(t *testing.T) {
	t.Parallel()

	plain := newQueryContext(dbContext{tableName: "torrent_contents"}).(optionBuilder)
	if plain.groupingSpec() != nil {
		t.Fatal("expected nil grouping spec on a fresh builder")
	}

	grouped := plain.GroupByDistinctOn(testGroupKeySQL, testGroupOrderSQL).(optionBuilder)
	if grouped.groupingSpec() == nil {
		t.Fatal("expected non-nil grouping spec after GroupByDistinctOn")
	}
	// The flat builder must be unaffected (options return copies).
	if plain.groupingSpec() != nil {
		t.Fatal("GroupByDistinctOn must not mutate the original builder")
	}
}
