package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmstage"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/query"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/releasefields"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestPostgresReleaseAttributesPersistenceFilteringNullAndDowngrade(t *testing.T) {
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("release_attributes_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	pcfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	require.NoError(t, err)
	sqlDB := stdlib.OpenDB(*pcfg.ConnConfig)
	t.Cleanup(func() {
		_ = sqlDB.Close()
		pool.Close()
		cleanup, e := pgx.Connect(context.Background(), dsn)
		if e == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	dq := dao.Use(gdb)
	proc := processor{dao: dq}
	name := "Amber.Signal.2025.2160p.HEVC.DV.HDR10.DDP5.1.Atmos.REPACK-GROUP.mkv"
	var hash protocol.ID
	hash[0] = 1
	_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,$2,73400320,false,'single',now(),now())`, hash.Bytes(), name)
	require.NoError(t, err)
	attrs := model.InferReleaseAttributes(name, "HEVC.DV.HDR10.DDP5.1.Atmos.REPACK")
	cl := classification.Result{ContentAttributes: classification.ContentAttributes{ContentType: model.NewNullContentType(model.ContentTypeMovie), ReleaseAttributes: attrs}}
	torrent := model.Torrent{InfoHash: hash, Name: name, Size: 73400320, FilesStatus: model.FilesStatusSingle}
	tc := newTorrentContent(torrent, cl, model.NullEnglishAudio{})
	require.NoError(t, proc.persist(ctx, persistPayload{torrentContents: []model.TorrentContent{tc}}))
	var retained []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT release_attributes FROM torrent_contents WHERE info_hash=$1`, hash.Bytes()).Scan(&retained))
	var decoded model.ReleaseAttributes
	require.NoError(t, json.Unmarshal(retained, &decoded))
	require.Equal(t, *attrs, decoded)
	// A later cycle may not reach the parser. The ordinary UpdateAll path must
	// retain non-null claims for the exact unchanged release name.
	noClaims := cl
	noClaims.ReleaseAttributes = nil
	tcNoClaims := newTorrentContent(torrent, noClaims, model.NullEnglishAudio{})
	require.NoError(t, proc.persist(ctx, persistPayload{torrentContents: []model.TorrentContent{tcNoClaims}}))
	require.NoError(t, pool.QueryRow(ctx, `SELECT release_attributes FROM torrent_contents WHERE info_hash=$1`, hash.Bytes()).Scan(&retained))
	require.JSONEq(t, string(mustReleaseJSON(t, attrs)), string(retained))
	_, err = pool.Exec(ctx, `UPDATE torrents SET name='Changed.Source.2025.mkv' WHERE info_hash=$1`, hash.Bytes())
	require.NoError(t, err)
	require.ErrorIs(t, proc.persist(ctx, persistPayload{torrentContents: []model.TorrentContent{tcNoClaims}}), errReleaseAttributesChanged)
	_, err = pool.Exec(ctx, `UPDATE torrents SET name=$2 WHERE info_hash=$1`, hash.Bytes(), name)
	require.NoError(t, err)

	backend, err := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return dq, nil })}).Search.Get()
	require.NoError(t, err)
	matched, err := backend.TorrentContent(ctx, query.Where(search.TorrentContentReleaseAttributeCriteria("hdrFormats", "DOLBY_VISION")))
	require.NoError(t, err)
	require.Len(t, matched.Items, 1)
	require.Equal(t, attrs, matched.Items[0].ReleaseAttributes)
	absent, err := backend.TorrentContent(ctx, query.Where(search.TorrentContentReleaseAttributeCriteria("audioFormats", "TRUEHD")))
	require.NoError(t, err)
	require.Empty(t, absent.Items)
	_, err = pool.Exec(ctx, `UPDATE torrent_contents SET release_attributes='{}'::jsonb WHERE info_hash=$1`, hash.Bytes())
	require.Error(t, err, "missing version/provenance cannot pass the storage contract")
	// Target the release-attribute migration even when newer additive
	// migrations have been applied; dropping those must not hide its guard.
	_, err = provider.DownTo(ctx, 56)
	require.Error(t, err, "downgrade must retain populated claim data")
	_, err = pool.Exec(ctx, `UPDATE torrent_contents SET release_attributes=NULL WHERE info_hash=$1`, hash.Bytes())
	require.NoError(t, err)
	_, err = provider.DownTo(ctx, 56)
	require.NoError(t, err)
}

func mustReleaseJSON(t *testing.T, v any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err)
	return body
}

func TestPostgresReleaseFieldRepairPreservesDeferredApplicationAfterTTL(t *testing.T) {
	h, work, lease, stage, pool, source, calls := deferredTypeHarness(t, nil, false, "Amber.Signal.2026.2160p.WEB-DL.HEVC.HDR10.DDP5.1.Atmos.REPACK.mkv")
	ctx := context.Background()
	require.NoError(t, h.Handle(llmwork.WithExecution(ctx, work, *lease), lease.Task))
	// Model the field facts of the previous parser without changing the type
	// receipt, source inputs, catalogue identity or application generation.
	_, err := pool.Exec(ctx, `UPDATE torrent_contents SET video_source='WEBRip',video_codec=NULL,release_attributes=NULL,updated_at=clock_timestamp();UPDATE llm_work_applications SET applied_snapshot=jsonb_set(jsonb_set(applied_snapshot-'releaseAttributes','{videoSource}','"WEBRip"'::jsonb),'{videoCodec}','null'::jsonb)`)
	require.NoError(t, err)
	plan, err := releasefields.Freeze(ctx, pool, []protocol.ID{source.InfoHash}, false)
	require.NoError(t, err)
	plan.Entries[0].SourceOwnershipEvidence = "reviewed synthetic legacy parser canary"
	_, err = releasefields.Apply(ctx, pool, plan, true)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM llm_evaluation_captures;UPDATE llm_work_tasks SET payload='{}'::jsonb`)
	require.NoError(t, err)
	p := llmstage.WorkPayload{Workflow: "default", Flags: classifier.Flags{"local_search_enabled": false}}
	a, err := work.Preserve(ctx, llmwork.Type, source, stage.WorkPolicy(p))
	require.NoError(t, err)
	require.NotNil(t, a)
	require.Equal(t, model.VideoSourceWEBDL, a.VideoSource.VideoSource)
	require.True(t, a.VideoCodec.Valid)
	require.Equal(t, model.VideoCodecHEVC, a.VideoCodec.VideoCodec)
	require.NotNil(t, a.ReleaseAttributes)
	require.Equal(t, []string{"HDR10"}, a.ReleaseAttributes.HDRFormats)
	// A repair rollback restores the application fact as well as the column,
	// even after the original HTTP body and task input have expired.
	_, err = releasefields.Rollback(ctx, pool, plan, true)
	require.NoError(t, err)
	a, err = work.Preserve(ctx, llmwork.Type, source, stage.WorkPolicy(p))
	require.NoError(t, err)
	require.False(t, a.VideoCodec.Valid)
	require.Equal(t, model.VideoSourceWEBRip, a.VideoSource.VideoSource)
	require.Nil(t, a.ReleaseAttributes)
	plan, err = releasefields.Freeze(ctx, pool, []protocol.ID{source.InfoHash}, false)
	require.NoError(t, err)
	plan.Entries[0].SourceOwnershipEvidence = "reviewed synthetic legacy parser canary"
	_, err = releasefields.Apply(ctx, pool, plan, true)
	require.NoError(t, err)
	sqlDB := stdlib.OpenDB(*pool.Config().ConnConfig)
	defer sqlDB.Close()
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	backend, err := h.Search.Get()
	require.NoError(t, err)
	ordinary := processor{dao: dao.Use(gdb), search: backend, runner: stage, defaultWorkflow: "default", logger: zap.NewNop().Sugar()}
	require.NoError(t, ordinary.Process(ctx, MessageParams{InfoHashes: []protocol.ID{source.InfoHash}, ClassifierFlags: p.Flags, SkipContentFilter: true}))
	require.EqualValues(t, 1, calls.Load())
	a, err = work.Preserve(ctx, llmwork.Type, source, stage.WorkPolicy(p))
	require.NoError(t, err)
	require.NotNil(t, a.ReleaseAttributes)
	require.True(t, a.VideoCodec.Valid)
	require.Equal(t, model.VideoCodecHEVC, a.VideoCodec.VideoCodec)
	_, err = releasefields.Rollback(ctx, pool, plan, true)
	require.ErrorIs(t, err, releasefields.ErrChanged, "a later ordinary refresh changes the frozen target revision")
}
