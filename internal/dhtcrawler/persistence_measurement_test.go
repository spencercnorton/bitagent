package dhtcrawler

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/protocol/metainfo"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type measuredBatches struct{ batches chan []infoHashWithMetaInfo }

func (m measuredBatches) In() chan<- infoHashWithMetaInfo    { return nil }
func (m measuredBatches) Out() <-chan []infoHashWithMetaInfo { return m.batches }

type measuredScrapes struct{ done chan nodeHasPeersForHash }

func (m measuredScrapes) In() chan<- nodeHasPeersForHash                       { return m.done }
func (m measuredScrapes) Run(context.Context, func(nodeHasPeersForHash)) error { return nil }

// This invokes the actual crawler metadata/pieces/source/queue transaction.
// Only network acquisition and the next tracker scrape are replaced by inputs.
// Measurements exclude schema creation and report the exact synthetic population.
func TestMeasuredCrawlerPersistence(t *testing.T) {
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL required")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, dsn)
	require.NoError(t, e)
	defer admin.Close(ctx)
	schema := fmt.Sprintf("crawl_measure_%d", time.Now().UnixNano())
	_, e = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, e)
	_, e = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, e)
	defer admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	cfg, e := pgxpool.ParseConfig(dsn)
	require.NoError(t, e)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, e)
	defer pool.Close()
	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer db.Close()
	migration, e := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, e)
	_, e = migration.Up(ctx)
	require.NoError(t, e)
	g, e := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, e)
	batches := measuredBatches{make(chan []infoHashWithMetaInfo, 1)}
	scrapes := measuredScrapes{make(chan nodeHasPeersForHash, 5000)}
	c := crawler{dao: dao.Use(g), savePieces: true, saveFilesThreshold: 256, persistTorrents: batches, scrape: scrapes, logger: zap.NewNop().Sugar(), persistedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{Name: "bitagent_test_persisted_total", Help: "Synthetic test counter"}, []string{"entity"})}
	active, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.runPersistTorrents(active) }()
	defer func() { cancel(); <-done }()
	for sample := 0; sample < 5; sample++ {
		batch := make([]infoHashWithMetaInfo, 5000)
		for i := range batch {
			var h protocol.ID
			binary.BigEndian.PutUint32(h[:4], uint32(sample*5000+i+1))
			batch[i] = infoHashWithMetaInfo{nodeHasPeersForHash: nodeHasPeersForHash{infoHash: h}, metaInfo: metainfo.Info{Name: "Synthetic.Amber.2024.1080p.WEB-DL.mkv", Length: 734003200, PieceLength: 16384, Pieces: make([]byte, 20)}}
		}
		started := time.Now()
		batches.batches <- batch
		for range batch {
			select {
			case <-scrapes.done:
			case <-time.After(10 * time.Second):
				t.Fatal("crawler did not persist and forward the metadata batch")
			}
		}
		elapsed := time.Since(started)
		t.Logf("measurement sample=%d torrents=5000 elapsed_ms=%.3f torrents_per_second=%.1f", sample+1, float64(elapsed.Microseconds())/1000, 5000/elapsed.Seconds())
	}
	for table, want := range map[string]int{"torrents": 25000, "torrent_pieces": 25000, "torrents_torrent_sources": 25000, "queue_jobs": 250} {
		var n int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n))
		require.Equal(t, want, n, table)
	}
}
