package junkpurge

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestQuarantineTombstoneLifecycle is the one runnable check behind the change
// that made quarantine expiry non-destructive. It walks the full lifecycle and
// fails if any leg of the reversibility guarantee breaks:
//
//	quarantine  -> torrent leaves `torrents` (expiry never did this work)
//	expire      -> row RETAINED, expired_at stamped, snapshot intact,
//	               NO torrent_liveness blacklist row written
//	list        -> tombstoned rows drop out of the operator review list
//	expire x2   -> idempotent; a tombstone is not re-stamped every cycle
//	re-acquire  -> a re-crawled, re-judged tombstone re-quarantines and the
//	               torrent is removed again (the ON CONFLICT DO UPDATE path;
//	               under DO NOTHING it would stay in search forever)
//
// Opt-in, matching TestBatchStoreIntegration: set BITAGENT_TEST_POSTGRES_DSN.
func TestQuarantineTombstoneLifecycle(t *testing.T) {
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to run PostgreSQL integration coverage")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("junkpurge_tombstone_test_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		cleanup, cleanupErr := pgx.Connect(context.Background(), dsn)
		if cleanupErr == nil {
			_, _ = cleanup.Exec(
				context.Background(),
				`DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`,
			)
			_ = cleanup.Close(context.Background())
		}
	})

	// torrents mirrors only the columns the quarantine snapshot round-trips.
	_, err = pool.Exec(ctx, `
CREATE TABLE torrents (
  info_hash bytea PRIMARY KEY,
  name text NOT NULL,
  size bigint NOT NULL DEFAULT 0,
  private boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  files_status text NOT NULL DEFAULT 'no_info',
  files_count integer
);
CREATE TABLE torrent_files (
  info_hash bytea NOT NULL,
  "index" integer NOT NULL,
  path text NOT NULL,
  size bigint NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE junkpurge_judgments (
  info_hash bytea PRIMARY KEY,
  verdict text NOT NULL,
  confidence real NOT NULL,
  torrent_name text,
  judged_at timestamptz NOT NULL DEFAULT now(),
  purged boolean NOT NULL DEFAULT false
);
-- The table expiry used to write into. It must stay EMPTY.
CREATE TABLE torrent_liveness (
  info_hash bytea PRIMARY KEY,
  status text NOT NULL,
  last_observed_at timestamptz,
  blacklisted_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);`)
	require.NoError(t, err)

	for _, name := range []string{
		"00029_junkpurge_quarantine.sql",
		"00047_junkpurge_quarantine_tombstone.sql",
	} {
		migration, readErr := migrationssql.FS.ReadFile(name)
		require.NoError(t, readErr)
		up := strings.SplitN(string(migration), "-- +goose Down", 2)[0]
		_, err = pool.Exec(ctx, up)
		require.NoError(t, err, "applying %s", name)
	}

	hash := []byte("0123456789abcdefghij") // 20 bytes
	seedTorrent := func(name string) {
		_, execErr := pool.Exec(ctx, `
INSERT INTO torrents (info_hash, name, size, files_count) VALUES ($1, $2, 4096, 1)`,
			hash, name)
		require.NoError(t, execErr)
		_, execErr = pool.Exec(ctx, `
INSERT INTO torrent_files (info_hash, "index", path, size) VALUES ($1, 0, $2, 4096)`,
			hash, name+"/file.mkv")
		require.NoError(t, execErr)
	}
	upsertJudgment := func(name string) {
		_, execErr := pool.Exec(ctx, `
INSERT INTO junkpurge_judgments (info_hash, verdict, confidence, torrent_name)
VALUES ($1, 'junk', 0.99, $2)
ON CONFLICT (info_hash) DO UPDATE SET torrent_name = excluded.torrent_name, purged = false`,
			hash, name)
		require.NoError(t, execErr)
	}
	quarantine := func() [][]byte {
		tx, txErr := pool.Begin(ctx)
		require.NoError(t, txErr)
		defer func() { _ = tx.Rollback(ctx) }()
		inserted, qErr := quarantineJunkTx(ctx, tx, [][]byte{hash}, 0.80)
		require.NoError(t, qErr)
		require.NoError(t, tx.Commit(ctx))
		return inserted
	}
	countTorrents := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrents`).Scan(&n))
		return n
	}
	backdate := func(days int) {
		_, execErr := pool.Exec(ctx, `
UPDATE junkpurge_quarantine SET quarantined_at = now() - make_interval(days => $1)`, days)
		require.NoError(t, execErr)
	}

	worker := &purgeWorker{
		cfg:     Config{QuarantineDays: 30},
		metrics: NewMetrics(),
		logger:  zap.NewNop().Sugar(),
		// verdicts stays nil: the ledger write is best-effort and nil-safe, and
		// this test is about what survives in junkpurge_quarantine.
	}

	// --- quarantine: the torrent leaves `torrents` here, not at expiry.
	seedTorrent("Some.Junk.Release.2019")
	upsertJudgment("Some.Junk.Release.2019")
	require.Len(t, quarantine(), 1)
	require.Zero(t, countTorrents(), "quarantine must remove the torrent from search")

	// --- expire: tombstone, do not destroy.
	backdate(40)
	worker.expireQuarantine(ctx, pool)

	var expiredAt *time.Time
	var snapshot, files []byte
	require.NoError(t, pool.QueryRow(ctx, `
SELECT expired_at, torrent_snapshot, files_snapshot FROM junkpurge_quarantine WHERE info_hash = $1`,
		hash).Scan(&expiredAt, &snapshot, &files),
		"expiry must RETAIN the quarantine row")
	require.NotNil(t, expiredAt, "expiry must stamp expired_at")
	require.NotEmpty(t, snapshot, "expiry must not discard torrent_snapshot")
	require.NotEmpty(t, files, "expiry must not discard files_snapshot")

	var blacklisted int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrent_liveness`).Scan(&blacklisted))
	require.Zero(t, blacklisted,
		"expiry must not blacklist — that is what forecloses re-crawl recovery")

	// --- the review list shows live rows only.
	items, total, err := ListQuarantine(ctx, pool, 30, 50, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, items)

	// --- idempotent: a second cycle must not re-stamp the tombstone. Without
	// the expired_at IS NULL guard this would also re-emit a ledger event per
	// row on every cycle, forever.
	firstStamp := *expiredAt
	worker.expireQuarantine(ctx, pool)
	require.NoError(t, pool.QueryRow(ctx, `
SELECT expired_at FROM junkpurge_quarantine WHERE info_hash = $1`, hash).Scan(&expiredAt))
	require.True(t, firstStamp.Equal(*expiredAt), "tombstone must not be re-stamped")

	// --- re-acquisition: the crawler brings the hash back and the model
	// re-judges it junk. It must re-quarantine, not silently no-op.
	seedTorrent("Some.Junk.Release.2019.REPACK")
	upsertJudgment("Some.Junk.Release.2019.REPACK")
	require.Equal(t, 1, countTorrents())
	require.Len(t, quarantine(), 1,
		"a re-acquired tombstone must re-quarantine (DO NOTHING would return 0 rows "+
			"and leave the junk torrent in search permanently)")
	require.Zero(t, countTorrents(), "re-quarantine must remove the torrent again")

	var name string
	require.NoError(t, pool.QueryRow(ctx, `
SELECT torrent_name, expired_at FROM junkpurge_quarantine WHERE info_hash = $1`,
		hash).Scan(&name, &expiredAt))
	require.Equal(t, "Some.Junk.Release.2019.REPACK", name, "snapshot must be refreshed")
	require.Nil(t, expiredAt, "re-quarantine must clear the tombstone and restart the window")

	items, total, err = ListQuarantine(ctx, pool, 30, 50, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, items, 1)
}
