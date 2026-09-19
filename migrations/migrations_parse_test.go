package migrationssql_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	migrationssql "github.com/spencercnorton/bitagent/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
	goose "github.com/pressly/goose/v3"
)

// TestAllMigrationsParse feeds every embedded migration file through goose's
// real SQL parser. goose parses lazily at Up time, so a malformed file crashes
// the migrator on BOOT in production instead of failing CI — exactly what
// happened 2026-07-10 when 00036 shipped with a comment that quoted a goose
// annotation verbatim (goose treats ANY line containing the marker as an
// annotation) and crash-looped Galactic-Torrent.
//
// The trick: Migration.UpContext parses the file BEFORE touching the DB, so
// running it against an unreachable DSN separates the two failure modes —
// parse errors (bug, fail the test) vs connection errors (expected, ignore).
func TestAllMigrationsParse(t *testing.T) {
	goose.SetBaseFS(migrationssql.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}

	migrations, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		t.Fatalf("collect migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations collected — embed or path broken")
	}

	// Lazy handle; never connects successfully.
	db, err := sql.Open("pgx", "postgres://127.0.0.1:1/parse_only?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, m := range migrations {
		err := m.UpContext(context.Background(), db)
		if err == nil {
			t.Fatalf("%s: Up unexpectedly succeeded against an unreachable DB", m.Source)
		}
		if strings.Contains(err.Error(), "failed to parse SQL migration file") {
			t.Errorf("%s: %v", m.Source, err)
		}
	}
}
