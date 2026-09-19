package search

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type captureStringArgument struct {
	value *string
}

func (capture captureStringArgument) Match(value driver.Value) bool {
	text, ok := value.(string)
	if ok {
		*capture.value = text
	}
	return ok
}

func TestTorrentCreatedAtWindowRendersIndexedJoinCount(t *testing.T) {
	t.Parallel()

	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}

	start := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	var countedSQL string
	mock.ExpectQuery(`SELECT count, cost, budget_exceeded from budgeted_count`).
		WithArgs(captureStringArgument{value: &countedSQL}, float64(5_000)).
		WillReturnRows(sqlmock.NewRows([]string{"count", "cost", "budget_exceeded"}).
			AddRow(42, 100, false))

	result, err := (&search{q: dao.Use(db)}).TorrentContent(
		context.Background(),
		query.Limit(0),
		query.WithTotalCount(true),
		query.WithAggregationBudget(5_000),
		TorrentContentCoreJoins(),
		query.Where(
			TorrentCreatedAfterCriteria(start),
			TorrentCreatedBeforeCriteria(end),
		),
	)
	if err != nil {
		t.Fatalf("torrent-content count: %v", err)
	}
	if result.TotalCount != 42 || result.TotalCountIsEstimate {
		t.Fatalf("count result = %#v, want exact 42", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}

	for _, fragment := range []string{
		`INNER JOIN "torrents"`,
		`"torrent_contents"."info_hash" = "torrents"."info_hash"`,
		`torrents.created_at >=`,
		`torrents.created_at <`,
	} {
		if !strings.Contains(countedSQL, fragment) {
			t.Errorf("count SQL missing %q:\n%s", fragment, countedSQL)
		}
	}
}
