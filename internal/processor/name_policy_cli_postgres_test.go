package processor_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/app/cmd/animebackfillcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/backloglocalclassifycmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/batchllmmatchcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/episodesbackfillcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/granularitybackfillcmd"
	"github.com/spencercnorton/bitagent/internal/app/cmd/matcherevalcmd"
	"github.com/spencercnorton/bitagent/internal/classifier"
	"github.com/spencercnorton/bitagent/internal/classifier/classification"
	"github.com/spencercnorton/bitagent/internal/classifier/llmmatch"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/llmwork"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/namepolicy"
	"github.com/spencercnorton/bitagent/internal/processor"
	"github.com/spencercnorton/bitagent/internal/protocol"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type noWorkRunner struct{ calls *atomic.Int32 }

func (r noWorkRunner) Run(context.Context, string, classifier.Flags, model.Torrent) (classification.Result, error) {
	r.calls.Add(1)
	return classification.Result{}, nil
}
func (r noWorkRunner) EvalMatch(context.Context, model.Torrent, model.NullContentType) (classifier.MatchDecision, error) {
	r.calls.Add(1)
	return classifier.MatchDecision{}, nil
}

type noWorkProcessor struct{ calls *atomic.Int32 }

func (p noWorkProcessor) Process(context.Context, processor.MessageParams) error {
	p.calls.Add(1)
	return nil
}

func cliPolicyFixture(t *testing.T) (*dao.Query, *pgxpool.Pool, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("cli_policy_%d", time.Now().UnixNano())
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
		db.Close()
		pool.Close()
		c, e := pgx.Connect(context.Background(), dsn)
		if e == nil {
			c.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			c.Close(context.Background())
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	g, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	return dao.Use(g), pool, g
}

func seedCLI(t *testing.T, pool *pgxpool.Pool, h protocol.ID, name string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at)VALUES($1,$2,1073741824,false,'multi',1,now(),now())`, h.Bytes(), name)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO torrent_contents(info_hash,content_type,size,is_anime,video_codec,created_at,updated_at)VALUES($1,'movie',1073741824,false,'h264',now(),now())`, h.Bytes())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at)VALUES($1,0,'Allowed.Release.mkv',1073741824,now(),now())`, h.Bytes())
	require.NoError(t, err)
}

func TestPostgresDirectCLIAndMaintenanceAnyAdultGateBeforeHydrationOrWork(t *testing.T) {
	for _, kind := range []string{"matcher-eval", "batch-llm-match", "backlog-local-classify", "anime-backfill", "episodes-backfill", "derived-backfill"} {
		t.Run(kind, func(t *testing.T) {
			d, pool, _ := cliPolicyFixture(t)
			ctx := context.Background()
			h := protocol.ID{1}
			seedCLI(t, pool, h, "Allowed.Stored.Release.mkv")
			_, err := pool.Exec(ctx, `INSERT INTO torrent_contents(info_hash,content_type,size,is_anime,created_at,updated_at)VALUES($1,'xxx',1073741824,false,now(),now())`, h.Bytes())
			require.NoError(t, err)
			seedCLI(t, pool, protocol.ID{2}, "Synthetic.电影.S20E048.ENG.mkv")
			_, err = pool.Exec(ctx, `UPDATE torrent_contents SET content_type='tv_show' WHERE info_hash=$1`, (protocol.ID{2}).Bytes())
			require.NoError(t, err)
			var before string
			require.NoError(t, pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(tc) ORDER BY id)::text FROM torrent_contents tc`).Scan(&before))
			// Any file hydration of a denied row must fail, rather than quietly
			// succeeding and giving a weak no-provider assertion.
			seedCLI(t, pool, protocol.ID{3}, "Allowed.Owner.Excluded.mkv")
			require.NoError(t, pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(tc) ORDER BY id)::text FROM torrent_contents tc`).Scan(&before))
			_, err = pool.Exec(ctx, `DROP TABLE torrent_files CASCADE`)
			require.NoError(t, err)
			p, err := namepolicy.New(namepolicy.Config{Enabled: true, ExcludedInfoHashes: []string{(protocol.ID{3}).String()}})
			require.NoError(t, err)
			calls := &atomic.Int32{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected", 500) }))
			defer server.Close()
			cfg := llmmatch.NewDefaultConfig()
			cfg.Enabled = true
			cfg.Endpoint = server.URL
			lm := llmmatch.NewClient(cfg, nil, llmmatch.NewMetrics(), zap.NewNop().Sugar())
			lm.SetNamePolicy(p)
			daoLazy := lazy.New(func() (*dao.Query, error) { return d, nil })
			runner := lazy.New(func() (classifier.Runner, error) { return noWorkRunner{calls}, nil })
			pr := lazy.New(func() (processor.Processor, error) { return noWorkProcessor{calls}, nil })
			var command *cli.Command
			switch kind {
			case "matcher-eval":
				r, e := matcherevalcmd.New(matcherevalcmd.Params{Dao: daoLazy, NamePolicy: p, Runner: runner, LLMMatch: lm, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = r.Command
			case "batch-llm-match":
				r, e := batchllmmatchcmd.New(batchllmmatchcmd.Params{Dao: daoLazy, NamePolicy: p, Processor: pr, LLMMatch: lm, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = r.Command
			case "backlog-local-classify":
				r, e := backloglocalclassifycmd.New(backloglocalclassifycmd.Params{Dao: daoLazy, NamePolicy: p, Processor: pr, Runner: runner, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = r.Command
			case "anime-backfill":
				r, e := animebackfillcmd.New(animebackfillcmd.Params{Dao: daoLazy, NamePolicy: p, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = r.Command
			case "episodes-backfill":
				r, e := episodesbackfillcmd.New(episodesbackfillcmd.Params{Dao: daoLazy, NamePolicy: p, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = r.Command
			case "derived-backfill":
				r, e := granularitybackfillcmd.New(granularitybackfillcmd.Params{Dao: daoLazy, NamePolicy: p, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = r.Command
			}
			args := []string{"owned-cli", kind, "--limit", "10"}
			if kind == "batch-llm-match" {
				args = append(args, "--llmBatchSize", "1")
			}
			if kind == "anime-backfill" || kind == "episodes-backfill" || kind == "derived-backfill" {
				args = append(args, "--write")
			}
			app := cli.NewApp()
			app.Writer = io.Discard
			app.ErrWriter = io.Discard
			app.Commands = []*cli.Command{command}
			require.NoError(t, app.RunContext(ctx, args))
			require.Zero(t, calls.Load())
			var after string
			require.NoError(t, pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(tc) ORDER BY id)::text FROM torrent_contents tc`).Scan(&after))
			require.Equal(t, before, after, "existing non-NULL codec/derived/classification facts remain unchanged")
		})
	}
}

func TestPostgresDirectCLIFinalAdmissionRejectsNameDriftAfterHydration(t *testing.T) {
	for _, kind := range []string{"matcher-eval", "batch-llm-match", "backlog-local-classify"} {
		t.Run(kind, func(t *testing.T) {
			d, pool, g := cliPolicyFixture(t)
			ctx := context.Background()
			h := protocol.ID{4}
			seedCLI(t, pool, h, "Allowed.Current.Release.mkv")
			var renamed atomic.Bool
			require.NoError(t, g.Callback().Query().After("gorm:query").Register("owned-name-drift", func(tx *gorm.DB) {
				if tx.Statement.Table == "torrent_files" && renamed.CompareAndSwap(false, true) {
					_, err := pool.Exec(ctx, `UPDATE torrents SET name='Synthetic.Фильм.ENG.mkv' WHERE info_hash=$1`, h.Bytes())
					require.NoError(t, err)
				}
			}))
			p, err := namepolicy.New(namepolicy.Config{Enabled: true})
			require.NoError(t, err)
			calls := &atomic.Int32{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected", 500) }))
			defer server.Close()
			cfg := llmmatch.NewDefaultConfig()
			cfg.Enabled = true
			cfg.Endpoint = server.URL
			lm := llmmatch.NewClient(cfg, nil, llmmatch.NewMetrics(), zap.NewNop().Sugar())
			lm.SetNamePolicy(p)
			dl := lazy.New(func() (*dao.Query, error) { return d, nil })
			r := lazy.New(func() (classifier.Runner, error) { return noWorkRunner{calls}, nil })
			pr := lazy.New(func() (processor.Processor, error) { return noWorkProcessor{calls}, nil })
			var command *cli.Command
			switch kind {
			case "matcher-eval":
				x, e := matcherevalcmd.New(matcherevalcmd.Params{Dao: dl, NamePolicy: p, Runner: r, LLMMatch: lm, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = x.Command
			case "batch-llm-match":
				x, e := batchllmmatchcmd.New(batchllmmatchcmd.Params{Dao: dl, NamePolicy: p, Processor: pr, LLMMatch: lm, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = x.Command
			case "backlog-local-classify":
				x, e := backloglocalclassifycmd.New(backloglocalclassifycmd.Params{Dao: dl, NamePolicy: p, Processor: pr, Runner: r, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = x.Command
			}
			app := cli.NewApp()
			app.Writer = io.Discard
			app.ErrWriter = io.Discard
			app.Commands = []*cli.Command{command}
			args := []string{"owned-cli", kind, "--limit", "10"}
			if kind == "batch-llm-match" {
				args = append(args, "--llmBatchSize", "1")
			}
			require.NoError(t, app.RunContext(ctx, args))
			require.True(t, renamed.Load(), "drift happened after the initial source predicate admitted the row")
			require.Zero(t, calls.Load(), "current source admission stops parser/matcher/provider/processor work after drift")
		})
	}
}

func TestPostgresMaintenanceSourceDriftCannotCommitDerivedUpdates(t *testing.T) {
	for _, kind := range []string{"anime-backfill", "episodes-backfill", "derived-backfill"} {
		t.Run(kind, func(t *testing.T) {
			d, pool, g := cliPolicyFixture(t)
			ctx := context.Background()
			h := protocol.ID{5}
			name := "Allowed.Current.Release.2024.1080p.mkv"
			if kind == "episodes-backfill" {
				name = "Allowed.Series.S20E048.1080p.mkv"
			}
			seedCLI(t, pool, h, name)
			_, err := pool.Exec(ctx, `UPDATE torrent_contents SET is_anime=true WHERE info_hash=$1`, h.Bytes())
			require.NoError(t, err)
			if kind == "episodes-backfill" {
				require.NoError(t, g.Model(&model.TorrentContent{}).Where("info_hash=?", h.Bytes()).UpdateColumns(map[string]interface{}{"content_type": "tv_show", "episodes": model.Episodes{20: {4: {}}}}).Error)
			}
			var before string
			require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(tc)::text FROM torrent_contents tc WHERE info_hash=$1`, h.Bytes()).Scan(&before))
			var renamed atomic.Bool
			require.NoError(t, g.Callback().Query().After("gorm:query").Register("owned-maintenance-drift", func(tx *gorm.DB) {
				if tx.Statement.Table == "torrents" && renamed.CompareAndSwap(false, true) {
					_, err := pool.Exec(ctx, `UPDATE torrents SET name='Synthetic.电影.S20E048.mkv' WHERE info_hash=$1`, h.Bytes())
					require.NoError(t, err)
				}
			}))
			p, err := namepolicy.New(namepolicy.Config{Enabled: true})
			require.NoError(t, err)
			dl := lazy.New(func() (*dao.Query, error) { return d, nil })
			var command *cli.Command
			switch kind {
			case "anime-backfill":
				x, e := animebackfillcmd.New(animebackfillcmd.Params{Dao: dl, NamePolicy: p, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = x.Command
			case "episodes-backfill":
				x, e := episodesbackfillcmd.New(episodesbackfillcmd.Params{Dao: dl, NamePolicy: p, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = x.Command
			case "derived-backfill":
				x, e := granularitybackfillcmd.New(granularitybackfillcmd.Params{Dao: dl, NamePolicy: p, Logger: zap.NewNop().Sugar()})
				require.NoError(t, e)
				command = x.Command
			}
			app := cli.NewApp()
			app.Writer = io.Discard
			app.ErrWriter = io.Discard
			app.Commands = []*cli.Command{command}
			require.ErrorIs(t, app.RunContext(ctx, []string{"owned-cli", kind, "--limit", "10", "--write"}), llmwork.ErrHeld)
			require.True(t, renamed.Load())
			var after string
			require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(tc)::text FROM torrent_contents tc WHERE info_hash=$1`, h.Bytes()).Scan(&after))
			require.Equal(t, before, after, "no old non-NULL codec/episodes/derived data is cleared after source drift")
		})
	}
}
