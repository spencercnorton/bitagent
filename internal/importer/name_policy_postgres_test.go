package importer

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/evidence"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/protocol"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func nameImportFixture(t *testing.T) (*activeImport, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("name_import_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() {
		_ = db.Close()
		pool.Close()
		c, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = c.Close(context.Background())
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	g, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	p, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)
	return &activeImport{importer: importer{dao: dao.Use(g), namePolicy: p, metrics: NewMetrics()}, ctx: ctx, importedSources: map[string]struct{}{}}, pool
}

func TestPostgresImportUsesStoredNameAndRollsBackConcurrentInsertAdmission(t *testing.T) {
	i, pool := nameImportFixture(t)
	ctx := context.Background()
	before := map[string]int{}
	for _, table := range []string{"torrent_sources", "torrents_torrent_sources", "torrent_hints", "queue_jobs"} {
		var n int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n))
		before[table] = n
	}
	h := protocol.ID{1}
	_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,'Synthetic.电影.ENG.mkv',1,false,'no_info',now(),now())`, h.Bytes())
	require.NoError(t, err)
	require.NoError(t, i.persistItems(Item{InfoHash: h, Name: "Allowed.Caller.Name.mkv", Source: "synthetic-source", Size: 1}))
	for _, table := range []string{"torrent_sources", "torrents_torrent_sources", "torrent_hints", "queue_jobs"} {
		var n int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n))
		require.Equal(t, before[table], n)
	}
	// Pause an allowed importer INSERT after its absent-row check. Another
	// writer inserts this hash with an excluded name. ON CONFLICT must not
	// authorize hints, source membership or processor work using caller text.
	_, err = pool.Exec(ctx, `CREATE FUNCTION pause_allowed_import() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.name='Allowed.Concurrent.Name.mkv' THEN PERFORM pg_advisory_xact_lock(793105); END IF; RETURN NEW; END$$; CREATE TRIGGER pause_allowed_import BEFORE INSERT ON torrents FOR EACH ROW EXECUTE FUNCTION pause_allowed_import()`)
	require.NoError(t, err)
	barrier, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer barrier.Release()
	_, err = barrier.Exec(ctx, `SELECT pg_advisory_lock(793105)`)
	require.NoError(t, err)
	defer barrier.Exec(context.Background(), `SELECT pg_advisory_unlock(793105)`)
	h = protocol.ID{2}
	finished := make(chan error, 1)
	go func() {
		finished <- i.persistItems(Item{InfoHash: h, Name: "Allowed.Concurrent.Name.mkv", Source: "synthetic-race-source", Size: 1})
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=793105 AND NOT granted)`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,'Synthetic.Фильм.ENG.mkv',1,false,'no_info',now(),now())`, h.Bytes())
	require.NoError(t, err)
	_, err = barrier.Exec(ctx, `SELECT pg_advisory_unlock(793105)`)
	require.NoError(t, err)
	require.NoError(t, <-finished)
	var name string
	require.NoError(t, pool.QueryRow(ctx, `SELECT name FROM torrents WHERE info_hash=$1`, h.Bytes()).Scan(&name))
	require.Equal(t, "Synthetic.Фильм.ENG.mkv", name)
	for _, table := range []string{"torrent_sources", "torrents_torrent_sources", "torrent_hints", "queue_jobs"} {
		var n int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n))
		require.Equal(t, before[table], n)
	}
	require.Empty(t, i.importedSources, "rolled-back source creation must not poison the importer cache")
}

func TestPostgresDirectPrivacyReaderNormalizesIndependentQBSourceAndCategory(t *testing.T) {
	_, pool := nameImportFixture(t)
	ctx := context.Background()
	hash := protocol.ID{3}
	store := evidence.NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	cases := []struct {
		source, category string
		private          bool
	}{
		{"qbittorrent", "private", true}, {"QBittorrent", "BiTgRaB", true},
		{" \tQBittorrent\n", " private ", true}, {"\u2003QBittorrent\u00a0", "\u2003BiTgRaB\u00a0", true},
		{"\u0085qbittorrent\u3000", "\u0085private\u3000", true}, {"qbittorrent", "public", false},
		{"radarr", "private", false}, {"qbittorrent-extra", "private", false},
	}
	for _, tc := range cases {
		t.Run(tc.source+"/"+tc.category, func(t *testing.T) {
			_, err := pool.Exec(ctx, `DELETE FROM label_evidence`)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO label_evidence(info_hash,source,category,source_kind,source_instance,source_object_id,observed_at,strength)VALUES($1,$2,$3,'test','test','test',now(),1)`, hash.Bytes(), tc.source, tc.category)
			require.NoError(t, err)
			private, err := store.IsPrivateInfoHash(ctx, hash.Bytes())
			require.NoError(t, err)
			require.Equal(t, tc.private, private)
		})
	}
}
