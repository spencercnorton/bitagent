package processor

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
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gen/field"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func deferredApplyFixture(t *testing.T) (*pgxpool.Pool, search.Search) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("deferred_apply_%d", time.Now().UnixNano())
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
		c, e := pgx.Connect(context.Background(), dsn)
		if e == nil {
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
	d := dao.Use(g)
	backend, err := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return d, nil })}).Search.Get()
	require.NoError(t, err)
	return pool, backend
}

func deferredTaskSource(t *testing.T, backend search.Search, hash protocol.ID) model.Torrent {
	t.Helper()
	r, err := backend.TorrentsWithMissingInfoHashes(context.Background(), []protocol.ID{hash}, query.Preload(func(d *dao.Query) []field.RelationField {
		return []field.RelationField{d.Torrent.Files.RelationField, d.Torrent.Hint.RelationField}
	}))
	require.NoError(t, err)
	require.Len(t, r.Torrents, 1)
	return r.Torrents[0]
}

func TestPostgresDeferredSourceAndTargetGuardRejectChangedPrivateBlockedAndManual(t *testing.T) {
	pool, backend := deferredApplyFixture(t)
	ctx := context.Background()
	var hash protocol.ID
	hash[0] = 1
	for _, statement := range []string{
		`INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,'Amber.Signal.1080p.mkv',73400320,false,'multi',1,now(),now())`,
		`INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,0,'Amber.Signal.1080p.mkv',73400320,now(),now())`,
		`INSERT INTO torrent_contents(info_hash,size,is_anime,created_at,updated_at) VALUES($1,73400320,false,now(),now())`,
	} {
		_, err := pool.Exec(ctx, statement, hash.Bytes())
		require.NoError(t, err)
	}
	source := deferredTaskSource(t, backend, hash)
	task := llmwork.Task{Draft: llmwork.Draft{InfoHash: hash.Bytes(), SourceDigest: llmwork.SourceDigest(source)}}
	check := func(wantObsolete bool) {
		tx, e := pool.Begin(ctx)
		require.NoError(t, e)
		defer tx.Rollback(ctx)
		current, e := lockedDeferredSource(ctx, tx, task)
		if wantObsolete {
			require.ErrorIs(t, e, llmwork.ErrObsolete)
			return
		}
		require.NoError(t, e)
		require.Equal(t, llmwork.SourceDigest(source), llmwork.SourceDigest(current))
		_, e = lockedUnknownDeferredTarget(ctx, tx, current, true)
		require.NoError(t, e)
	}
	check(false)
	_, err := pool.Exec(ctx, "UPDATE torrents SET name='Changed.Source.mkv' WHERE info_hash=$1", hash.Bytes())
	require.NoError(t, err)
	check(true)
	_, err = pool.Exec(ctx, "UPDATE torrents SET name=$2,private=true WHERE info_hash=$1", hash.Bytes(), source.Name)
	require.NoError(t, err)
	check(true)
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=false WHERE info_hash=$1", hash.Bytes())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO label_evidence(info_hash,source,category,source_kind,source_instance,source_object_id,observed_at,strength) VALUES($1,'qbittorrent','private','test','test','test',now(),1)`, hash.Bytes())
	require.NoError(t, err)
	check(true)
	_, err = pool.Exec(ctx, "DELETE FROM label_evidence WHERE info_hash=$1", hash.Bytes())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO torrent_hints(info_hash,content_type,created_at,updated_at) VALUES($1,'movie',now(),now())`, hash.Bytes())
	require.NoError(t, err)
	check(true)
	_, err = pool.Exec(ctx, "DELETE FROM torrent_hints WHERE info_hash=$1", hash.Bytes())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE torrent_contents SET content_type='movie' WHERE info_hash=$1`, hash.Bytes())
	require.NoError(t, err)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	current, err := lockedDeferredSource(ctx, tx, task)
	require.NoError(t, err)
	_, err = lockedUnknownDeferredTarget(ctx, tx, current, true)
	require.ErrorIs(t, err, llmwork.ErrObsolete, "a known type cannot be replaced by deferred type output")
}
