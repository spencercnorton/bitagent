package releasefields

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
)

func repairFixture(t *testing.T) (*pgxpool.Pool, protocol.ID) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;ALTER EXTENSION pg_trgm SET SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;ALTER EXTENSION btree_gin SET SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)
	schema := fmt.Sprintf("release_repair_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	admin.Close(ctx)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Close()
		pool.Close()
		c, e := pgx.Connect(context.Background(), dsn)
		if e == nil {
			c.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			c.Close(context.Background())
		}
	})
	hash := protocol.ID{31}
	_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)VALUES($1,'Amber.Signal.2026.2160p.WEB-DL.HEVC.HDR10.DDP5.1.Atmos.REPACK.mkv',734003200,false,'single',now(),now())`, hash.Bytes())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO torrent_contents(info_hash,content_type,video_source,size,is_anime,created_at,updated_at)VALUES($1,'movie','WEBDL',734003200,false,now(),now())`, hash.Bytes())
	require.NoError(t, err)
	return pool, hash
}

func TestPostgresFrozenNullOnlyRepairResumeAndRollback(t *testing.T) {
	pool, hash := repairFixture(t)
	ctx := context.Background()
	plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err)
	require.Len(t, plan.Entries, 1)
	require.Nil(t, plan.Entries[0].Before.ReleaseAttributes)
	require.NotNil(t, plan.Entries[0].After.ReleaseAttributes)
	out, err := Apply(ctx, pool, plan, false)
	require.NoError(t, err)
	require.Equal(t, "would_apply", out[0].State)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := Apply(ctx, pool, plan, true); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM release_field_repair_journal`).Scan(&count))
	require.Equal(t, 1, count)
	out, err = Apply(ctx, pool, plan, true)
	require.NoError(t, err)
	require.Equal(t, "already_applied", out[0].State)
	out, err = Rollback(ctx, pool, plan, false)
	require.NoError(t, err)
	require.Equal(t, "would_restore", out[0].State)
	out, err = Rollback(ctx, pool, plan, true)
	require.NoError(t, err)
	require.Equal(t, "restored", out[0].State)
	var raw []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT release_attributes FROM torrent_contents`).Scan(&raw))
	require.Empty(t, raw)
	var nullProjection bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT tsv IS NULL FROM torrent_contents`).Scan(&nullProjection))
	require.True(t, nullProjection)
}
func TestPostgresRepairDeclinesSourceManualVersionAndNonNullAttributeEdits(t *testing.T) {
	for _, mutation := range []string{`UPDATE torrents SET name='Changed.Source.mkv'`, `UPDATE torrent_contents SET updated_at=updated_at+interval '1 second'`, `INSERT INTO torrent_hints(info_hash,content_type,created_at,updated_at)VALUES($1,'movie',now(),now())`, `INSERT INTO torrent_tags(info_hash,name,created_at,updated_at)VALUES($1,'reference',now(),now())`, `INSERT INTO torrent_canonical_labels(info_hash,media_type,media_id,resolved_source,resolved_at,resolved_strength)VALUES($1,'movie','tmdb:42','radarr',now(),1)`} {
		t.Run(mutation, func(t *testing.T) {
			pool, hash := repairFixture(t)
			ctx := context.Background()
			plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
			require.NoError(t, err)
			if strings.Contains(mutation, "$1") {
				_, err = pool.Exec(ctx, mutation, hash.Bytes())
			} else {
				_, err = pool.Exec(ctx, mutation)
			}
			require.NoError(t, err)
			_, err = Apply(ctx, pool, plan, true)
			require.ErrorIs(t, err, ErrChanged)
		})
	}
	pool, hash := repairFixture(t)
	ctx := context.Background()
	plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err)
	body, _ := json.Marshal(plan.Entries[0].After.ReleaseAttributes)
	_, err = pool.Exec(ctx, `UPDATE torrent_contents SET release_attributes=$1::jsonb`, string(body))
	require.NoError(t, err)
	plan, err = Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err)
	plan.Entries[0].After.ReleaseAttributes = &model.ReleaseAttributes{Version: 1, Parser: model.ReleaseAttributesParser, SourceNameSHA256: plan.Entries[0].SourceNameSHA256, AudioFormats: []string{"TRUEHD"}}
	_, err = Apply(ctx, pool, plan, true)
	require.ErrorIs(t, err, ErrChanged)
}
func TestPostgresReviewedSourceCorrectionKeepsApplicationSnapshotAndRollback(t *testing.T) {
	pool, hash := repairFixture(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE torrent_contents SET video_source='WEBRip'`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO content(type,source,id,title,release_year,tsv,created_at,updated_at)VALUES('movie','tmdb','42','Amber Signal',2026,to_tsvector('simple','Amber Signal'),now(),now());UPDATE torrent_contents SET content_source='tmdb',content_id='42',tsv=to_tsvector('simple','Amber Signal WEBRip')`)
	require.NoError(t, err)
	task := digest("synthetic application")
	_, err = pool.Exec(ctx, `INSERT INTO llm_work_tasks(task_key,kind,info_hash,source_digest,policy_digest,input_digest,family_digest,payload,priority,time_bucket,daily_limit,monthly_limit,state,expires_at,completed_at) VALUES($1,'classifier_type',$2,$1,$1,$1,$1,'{}',0,0,0,0,'completed',now()+interval '1 day',now())`, task, hash.Bytes())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO llm_work_applications(task_key,info_hash,source_digest,policy_digest,applied_snapshot)VALUES($1,$2,$1,$1,'{"schema":"llm-application-v1","videoSource":"WEBRip"}'::jsonb)`, task, hash.Bytes())
	require.NoError(t, err)
	plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err)
	_, err = Apply(ctx, pool, plan, true)
	require.ErrorIs(t, err, ErrChanged, "a non-null old source is not assumed parser-owned")
	plan.Entries[0].SourceOwnershipEvidence = "reviewed synthetic canary"
	_, err = Apply(ctx, pool, plan, true)
	require.NoError(t, err)
	var source string
	require.NoError(t, pool.QueryRow(ctx, `SELECT applied_snapshot->>'videoSource' FROM llm_work_applications`).Scan(&source))
	require.Equal(t, "WEBDL", source)
	var wrong, title bool
	var id string
	require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(tsv@@plainto_tsquery('simple','WEBRip'),false),coalesce(tsv@@plainto_tsquery('simple','Amber Signal'),false),content_id FROM torrent_contents`).Scan(&wrong, &title, &id))
	require.False(t, wrong)
	require.True(t, title)
	require.Equal(t, "42", id)
	_, err = Rollback(ctx, pool, plan, true)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT applied_snapshot->>'videoSource' FROM llm_work_applications`).Scan(&source))
	require.Equal(t, "WEBRip", source)
	require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(tsv@@plainto_tsquery('simple','WEBRip'),false),coalesce(tsv@@plainto_tsquery('simple','Amber Signal'),false),content_id FROM torrent_contents`).Scan(&wrong, &title, &id))
	require.True(t, wrong)
	require.True(t, title)
	require.Equal(t, "42", id)
}
func TestPostgresRepairJournalFailureRollsBackFields(t *testing.T) {
	pool, hash := repairFixture(t)
	ctx := context.Background()
	plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `CREATE FUNCTION reject_repair_journal() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'synthetic journal failure';END$$;CREATE TRIGGER reject_repair_journal BEFORE INSERT ON release_field_repair_journal FOR EACH ROW EXECUTE FUNCTION reject_repair_journal()`)
	require.NoError(t, err)
	_, err = Apply(ctx, pool, plan, true)
	require.ErrorContains(t, err, "synthetic journal failure")
	var raw []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT release_attributes FROM torrent_contents`).Scan(&raw))
	require.Empty(t, raw)
	var codec model.NullVideoCodec
	require.NoError(t, pool.QueryRow(ctx, `SELECT video_codec FROM torrent_contents`).Scan(&codec))
	require.False(t, codec.Valid, "journal failure must roll back the codec too")
}

func TestPostgresRepairQualifiedProtectionRefusesFreezeAndInterveningApply(t *testing.T) {
	pool, hash := repairFixture(t)
	ctx := context.Background()
	plan, err := Freeze(ctx, pool, []protocol.ID{hash}, false)
	require.NoError(t, err)
	snapshot := func() map[string]string {
		out := map[string]string{}
		for _, table := range []string{"torrents", "torrent_files", "torrent_contents", "torrent_tags", "llm_work_applications", "release_field_repair_journal"} {
			var raw string
			require.NoError(t, pool.QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(r) order by to_jsonb(r)::text),'[]'::jsonb)::text from `+pgx.Identifier{table}.Sanitize()+` r`).Scan(&raw))
			out[table] = raw
		}
		return out
	}

	for _, prefix := range []string{"wanted", "manual", "reference", "bitgrab"} {
		name := prefix + "-synthetic"
		_, err := pool.Exec(ctx, `insert into torrent_tags(info_hash,name,created_at,updated_at) values($1,$2,now(),now())`, hash.Bytes(), name)
		require.NoError(t, err)
		before := snapshot()
		_, err = Freeze(ctx, pool, []protocol.ID{hash}, false)
		require.ErrorIs(t, err, ErrChanged)
		_, err = Apply(ctx, pool, plan, true)
		require.ErrorIs(t, err, ErrChanged)
		require.Equal(t, before, snapshot(), "native-format prefixed protection must retain raw and journals")
		_, err = pool.Exec(ctx, `delete from torrent_tags where info_hash=$1 and name=$2`, hash.Bytes(), name)
		require.NoError(t, err)
	}
	// Broaden only this owned synthetic fixture to cover legacy/imported tag
	// spellings; production formatting constraints remain untouched.
	_, err = pool.Exec(ctx, `ALTER TABLE torrent_tags DROP CONSTRAINT torrent_tags_name_check`)
	require.NoError(t, err)
	for _, prefix := range []string{"wanted", "manual", "reference", "bitgrab"} {
		for _, separator := range []string{":", "/", "-", "_"} {
			name := " \t" + strings.ToUpper(prefix) + separator + "synthetic\r\n"
			t.Run(prefix+separator, func(t *testing.T) {
				_, err := pool.Exec(ctx, `insert into torrent_tags(info_hash,name,created_at,updated_at) values($1,$2,now(),now())`, hash.Bytes(), name)
				require.NoError(t, err)
				before := snapshot()
				_, err = Freeze(ctx, pool, []protocol.ID{hash}, false)
				require.ErrorIs(t, err, ErrChanged)
				for _, write := range []bool{false, true} {
					_, err = Apply(ctx, pool, plan, write)
					require.ErrorIs(t, err, ErrChanged)
				}
				require.Equal(t, before, snapshot(), "protected freeze/apply cannot alter raw identity, nullable fields or ledgers")
				_, err = pool.Exec(ctx, `delete from torrent_tags where info_hash=$1 and name=$2`, hash.Bytes(), name)
				require.NoError(t, err)
			})
		}
	}
}
