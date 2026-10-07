package search

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Every connection in this test-owned pool starts with the selected setting.
// A cancelled PostgreSQL query may retire a physical connection; its replacement
// must also retain the caller's configured default, not inherit a prefix SET.
func groupedPrefixJitFixture(t *testing.T, jit string) (adultServingPostgresFixture, *gorm.DB, *groupedPrefixTrace) {
	t.Helper()
	f := newAdultServingPostgresFixture(t)
	var schema string
	require.NoError(t, f.q.TorrentContent.WithContext(context.Background()).UnderlyingDB().Raw("SELECT current_schema()").Scan(&schema).Error)
	cfg, err := pgx.ParseConfig(os.Getenv("BITAGENT_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	cfg.RuntimeParams["jit"] = jit
	sqlDB := stdlib.OpenDB(*cfg)
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	trace := &groupedPrefixTrace{Interface: gormlogger.Discard}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: trace})
	require.NoError(t, err)
	f.q = dao.Use(db)
	return f, db, trace
}

type groupedPrefixJitWitness struct {
	Jit      string
	ReadOnly string
	TxBound  bool
}

func groupedPrefixJitSettings(db *gorm.DB) (groupedPrefixJitWitness, error) {
	var result groupedPrefixJitWitness
	err := db.Session(&gorm.Session{NewDB: true}).Raw(
		"SELECT current_setting('jit') AS jit, current_setting('transaction_read_only') AS read_only").Scan(&result).Error
	_, result.TxBound = db.Statement.ConnPool.(gorm.TxCommitter)
	return result, err
}

func groupedPrefixJitOptions(hydrate bool) []query.Option {
	options := []query.Option{TorrentContentGroupByContentOption(), TorrentContentCoreJoins(),
		query.WithTotalCount(false),
		query.OrderBy(TorrentContentOrderByName.Clauses(OrderDirectionAscending)...),
		query.Limit(1), query.WithHasNextPage(false)}
	if hydrate {
		options = append(options, HydrateTorrentContentTorrent())
	}
	return options
}

func TestGroupedPrefixPostgresJitOwnedTransactionRestoresPool(t *testing.T) {
	for _, prior := range []string{"on", "off"} {
		for _, outcome := range []string{"success", "sql_error", "cancelled"} {
			t.Run(prior+"/"+outcome, func(t *testing.T) {
				f, db, trace := groupedPrefixJitFixture(t, prior)
				before, err := groupedPrefixJitSettings(db)
				require.NoError(t, err)
				require.Equal(t, prior, before.Jit)
				require.Equal(t, "off", before.ReadOnly)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var mtx sync.Mutex
				var witnesses []groupedPrefixJitWitness
				var witnessError error
				require.NoError(t, db.Callback().Row().Before("gorm:row").Register("test:grouped_prefix_jit", func(tx *gorm.DB) {
					if !strings.HasPrefix(strings.TrimSpace(tx.Statement.SQL.String()), "WITH eligible AS NOT MATERIALIZED") {
						return
					}
					witness, settingErr := groupedPrefixJitSettings(tx)
					mtx.Lock()
					witnesses = append(witnesses, witness)
					witnessError = settingErr
					mtx.Unlock()
					if settingErr != nil {
						tx.AddError(settingErr)
						return
					}
					switch outcome {
					case "sql_error":
						// A real PostgreSQL error aborts this transaction after
						// SET LOCAL. Cleanup must release its setting and pool.
						tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Exec("SELECT 1/0").Error)
					case "cancelled":
						cancel()
					}
				}))
				result, searchErr := f.serving(t, true).TorrentContent(ctx, groupedPrefixJitOptions(true)...)
				if outcome == "success" {
					require.NoError(t, searchErr)
					require.Len(t, result.Items, 1)
					require.Equal(t, f.hashes[4], result.Items[0].InfoHash)
				} else {
					require.Error(t, searchErr)
					if outcome == "cancelled" {
						require.True(t, errors.Is(searchErr, context.Canceled))
					}
				}
				mtx.Lock()
				require.NoError(t, witnessError)
				require.NotEmpty(t, witnesses, "the actual prefix Row callback must run")
				for _, witness := range witnesses {
					require.True(t, witness.TxBound)
					require.Equal(t, "off", witness.Jit)
					require.Equal(t, "on", witness.ReadOnly)
				}
				mtx.Unlock()
				require.Positive(t, trace.prefixes.Load())
				after, err := groupedPrefixJitSettings(db.WithContext(context.Background()))
				require.NoError(t, err, "the pool remains usable after rollback/cancellation")
				require.Equal(t, prior, after.Jit)
				require.Equal(t, "off", after.ReadOnly)
			})
		}
	}
}

func TestGroupedPrefixPostgresJitCallerTransactionRetainsVisibilityAndSettings(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		for _, scoped := range []bool{false, true} {
			t.Run(map[bool]string{false: "plain", true: "prepared"}[prepared]+"/"+map[bool]string{false: "caller_transaction_visibility", true: "deferred_scope_setting_safety"}[scoped], func(t *testing.T) {
				f, db, trace := groupedPrefixJitFixture(t, "on")
				if scoped {
					// The legacy outer factory does not inherit an inner-only
					// deferred connection scope. Give that unchanged fallback
					// its own connection; this case proves setting isolation,
					// not visibility that the legacy path never provided.
					pool, err := db.DB()
					require.NoError(t, err)
					pool.SetMaxOpenConns(2)
					pool.SetMaxIdleConns(2)
				}
				tx := db.Begin()
				require.NoError(t, tx.Error)
				t.Cleanup(func() { _ = tx.Rollback().Error })
				if prepared {
					tx = tx.Session(&gorm.Session{PrepareStmt: true})
				}
				require.NoError(t, tx.Exec("SET LOCAL jit=off").Error)
				require.NoError(t, tx.Exec("UPDATE torrent_contents SET seeders=20000 WHERE info_hash=?", f.hashes[4].Bytes()).Error)
				name := "Aardvark.Uncommitted.Caller.Release.mkv"
				require.NoError(t, tx.Exec("UPDATE torrents SET name=? WHERE info_hash=?", name, f.hashes[4].Bytes()).Error)
				var outside string
				require.NoError(t, f.pool.QueryRow(context.Background(), "SELECT name FROM torrents WHERE info_hash=$1", f.hashes[4].Bytes()).Scan(&outside))
				require.NotEqual(t, name, outside, "the marker remains uncommitted")
				before, err := groupedPrefixJitSettings(tx)
				require.NoError(t, err)
				require.True(t, before.TxBound)
				require.Equal(t, "off", before.Jit)
				// The deferred factory scope affects the primary query. Its
				// unrelated hydration callback still belongs to the base DAO;
				// direct caller DAOs separately prove hydrated visibility.
				options := groupedPrefixJitOptions(!scoped)
				if scoped {
					options = append(options, func(b query.OptionBuilder) (query.OptionBuilder, error) {
						return b.Scope(func(selected *gorm.DB) error {
							selected.Statement.ConnPool = tx.Statement.ConnPool
							return nil
						}), nil
					})
				} else {
					f.q = dao.Use(tx)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				result, err := f.serving(t, true).TorrentContent(ctx, options...)
				require.NoError(t, err)
				require.Len(t, result.Items, 1)
				require.Equal(t, f.hashes[4], result.Items[0].InfoHash)
				require.True(t, result.Items[0].Seeders.Valid)
				if !scoped {
					require.EqualValues(t, 20000, result.Items[0].Seeders.Uint)
					require.Equal(t, name, result.Items[0].Torrent.Name)
				} else {
					require.EqualValues(t, 10, result.Items[0].Seeders.Uint, "retain the legacy scope-only fallback's committed view")
				}
				require.Zero(t, trace.prefixes.Load(), "plain and prepared caller transactions must keep the original path")
				after, err := groupedPrefixJitSettings(tx)
				require.NoError(t, err)
				require.Equal(t, before, after)
				require.NoError(t, tx.Rollback().Error)
				poolSettings, err := groupedPrefixJitSettings(db)
				require.NoError(t, err)
				require.Equal(t, "on", poolSettings.Jit)
			})
		}
	}
}
