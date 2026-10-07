package cataloguerecovery_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/csamblocklist"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/gql/gqlmodel"
	"github.com/spencercnorton/bitagent/internal/gql/resolvers"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/tmdb"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Exercise the real CEL delete action, processor, shared writer and complete
// source restore. There are no remote providers or deployment-specific inputs.
func TestPostgresLegacyCELWriterRecoveryOffProtectedAndCaptured(t *testing.T) {
	for _, mode := range []string{"off", "protected", "captured"} {
		t.Run(mode, func(t *testing.T) {
			pool := recoveryPool(t)
			h := hash(1)
			seed(t, pool, h, false)
			id, err := protocol.NewIDFromByteSlice(h)
			require.NoError(t, err)
			lp := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
			manager, err := blocking.New(blocking.Params{Pool: lp, PgxPoolWait: &sync.WaitGroup{}}).Manager.Get()
			require.NoError(t, err)
			var recovery *cataloguerecovery.Store
			if mode != "off" {
				recovery = store(t, pool)
				manager, err = blocking.WithRecovery(manager, recovery)
				require.NoError(t, err)
			}
			if mode == "protected" {
				_, err = pool.Exec(ctx, `insert into torrent_tags(info_hash,name,created_at,updated_at) values($1,'wanted',now(),now())`, h)
				require.NoError(t, err)
			}
			before := raw(t, pool, h)
			sqlDB := stdlib.OpenDB(*pool.Config().ConnConfig)
			t.Cleanup(func() { _ = sqlDB.Close() })
			gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlogger.Discard})
			require.NoError(t, err)
			dq := dao.Use(gdb)
			ld := lazy.New(func() (*dao.Query, error) { return dq, nil })
			backend := search.New(search.Params{Query: ld}).Search
			classifierConfig := classifier.NewDefaultConfig()
			classifierConfig.Workflow = "synthetic_delete"
			compiler, err := classifier.New(classifier.Params{Config: classifierConfig, Search: backend, TmdbClient: lazy.New(func() (tmdb.Client, error) { return nil, nil })}).Compiler.Get()
			require.NoError(t, err)
			var source classifier.Source
			require.NoError(t, json.Unmarshal([]byte(`{"workflows":{"synthetic_delete":"delete"}}`), &source))
			runner, err := compiler.Compile(source)
			require.NoError(t, err)
			deleteMetrics := processor.NewDeleteMetrics()
			p, err := processor.New(processor.Params{
				ClassifierConfig: classifierConfig, Search: backend, Dao: ld,
				Workflow:        lazy.New(func() (classifier.Runner, error) { return runner, nil }),
				BlockingManager: lazy.New(func() (blocking.Manager, error) { return manager, nil }),
				CsamExporter:    csamblocklist.NewExporter(csamblocklist.Config{}, zap.NewNop().Sugar(), nil),
				DeleteMetrics:   deleteMetrics, Logger: zap.NewNop().Sugar(),
			}).Processor.Get()
			require.NoError(t, err)
			err = p.Process(ctx, processor.MessageParams{InfoHashes: []protocol.ID{id}})
			if mode != "captured" {
				want := cataloguerecovery.ErrDisabled
				if mode == "protected" {
					want = cataloguerecovery.ErrProtected
				}
				require.ErrorIs(t, err, want)
				require.Equal(t, before, raw(t, pool, h))
				for _, table := range []string{"catalogue_recovery_snapshots", "catalogue_recovery_events", "torrent_verdict_state", "torrent_verdict_events", "bloom_filters"} {
					require.Zero(t, count(t, pool, table), table)
				}
				for _, collector := range deleteMetrics.Collectors() {
					require.Zero(t, testutil.CollectAndCount(collector))
				}
				// The direct GraphQL action must use exactly the same guard.
				resolver := resolvers.Resolver{BlockingManager: manager}
				_, err = resolver.TorrentMutation().Delete(ctx, &gqlmodel.TorrentMutation{}, []protocol.ID{id})
				require.ErrorIs(t, err, want)
				require.Equal(t, before, raw(t, pool, h))
				return
			}
			require.NoError(t, err)
			require.Zero(t, count(t, pool, "torrents"))
			var snapshotID int64
			require.NoError(t, pool.QueryRow(ctx, `select id from catalogue_recovery_snapshots where info_hash=$1`, h).Scan(&snapshotID))
			require.Equal(t, 1, count(t, pool, "torrent_verdict_events"), "no independent blacklist may invalidate the receipt")
			ok, err := recovery.Restore(ctx, snapshotID, time.Now())
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, before, raw(t, pool, h))
		})
	}
}
