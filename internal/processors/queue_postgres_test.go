package processors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	v1 "github.com/spencercnorton/bitagent/processorapi/v1"
	"github.com/stretchr/testify/require"
)

var testContext = context.Background()

func fixture(t *testing.T) (*Store, *pgxpool.Pool, *goose.Provider) { return fixtureAt(t, 0) }
func fixtureAt(t *testing.T, version int64) (*Store, *pgxpool.Pool, *goose.Provider) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	ctx := testContext
	admin, e := pgx.Connect(ctx, dsn)
	require.NoError(t, e)
	_, e = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, e)
	schema := fmt.Sprintf("processors_%d", time.Now().UnixNano())
	_, e = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, e)
	cfg, e := pgxpool.ParseConfig(dsn)
	require.NoError(t, e)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, e)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	provider, e := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, e)
	if version > 0 {
		_, e = provider.UpTo(ctx, version)
	} else {
		_, e = provider.Up(ctx)
	}
	require.NoError(t, e)
	t.Cleanup(func() {
		db.Close()
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(ctx)
	})
	conf := NewDefaultConfig()
	conf.RequestTimeout = 150 * time.Millisecond
	conf.MaxAttempts = 2
	conf.Concurrency = 1
	s, e := NewStore(conf, lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }), "synthetic-core-v1")
	require.NoError(t, e)
	return s, pool, provider
}
func seedTorrent(t *testing.T, p *pgxpool.Pool, n byte, kind any) []byte {
	t.Helper()
	h := protocol.ID{n}.Bytes()
	_, e := p.Exec(testContext, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)VALUES($1,'Synthetic.Amber.2024.1080p.WEB-DL.H264-GROUP.mkv',734003200,false,'single',now(),now());
 INSERT INTO torrent_contents(info_hash,content_type,size,created_at,updated_at)VALUES($1,$2,734003200,now(),now());`, h, kind)
	require.NoError(t, e)
	return h
}

type service struct {
	server   *httptest.Server
	identity v1.Identity
	calls    atomic.Int64
}

func newService(t *testing.T, id string, stages []v1.Stage, fn func(v1.Job) v1.Result) *service {
	s := &service{identity: v1.Identity{ID: id, Version: "1.0.0", ModelVersion: "none", PolicyVersion: "test-v1", ArtifactDigest: "none"}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/manifest":
			_ = json.NewEncoder(w).Encode(v1.Manifest{Contract: v1.Contract, Processor: s.identity, Capabilities: stages, ConfigurationSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`), Qualification: "synthetic"})
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(v1.ServiceStatus{Contract: v1.Contract, Processor: s.identity, State: "ready", InstanceID: "test-instance", StartedAt: "2026-01-01T00:00:00Z"})
		case "/v1/process":
			s.calls.Add(1)
			var j v1.Job
			_ = json.NewDecoder(r.Body).Decode(&j)
			result := fn(j)
			_ = json.NewEncoder(w).Encode(result)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}
func (s *service) write() v1.RegistryWrite {
	return v1.RegistryWrite{Endpoint: s.server.URL, Mode: "apply", State: "active", Configuration: json.RawMessage(`{}`)}
}
func result(j v1.Job, kind string) v1.Result {
	r := v1.Result{Binding: j.Binding, Outcome: v1.Completed, Proposal: &v1.Proposal{ContentType: kind}, Reasons: []string{}, Evidence: []v1.Evidence{}}
	return r
}
func register(t *testing.T, s *Store, p *service) Entry {
	t.Helper()
	x, e := s.Register(testContext, p.identity.ID, p.write())
	require.NoError(t, e)
	return x
}
func run(t *testing.T, s *Store) {
	t.Helper()
	ok, e := s.RunOne(testContext)
	require.NoError(t, e)
	require.True(t, ok)
}

func TestPostgresZeroPluginsAndDeterministicApplication(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 1, nil)
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	ok, e := s.RunOne(testContext)
	require.NoError(t, e)
	require.False(t, ok)
	status, e := s.Status(testContext)
	require.NoError(t, e)
	require.Empty(t, status.Processors)
	require.Equal(t, "ready", status.CoreReadiness)
	a := newService(t, "alpha", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	register(t, s, a)
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n)
	run(t, s)
	var kind, codec, source string
	e = p.QueryRow(testContext, `SELECT content_type,video_codec,video_source FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind, &codec, &source)
	require.NoError(t, e)
	require.Equal(t, "movie", kind)
	require.Equal(t, "H264", codec)
	require.Equal(t, "WEBDL", source)
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	require.EqualValues(t, 1, a.calls.Load())
	var apps int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_applications`).Scan(&apps))
	require.Equal(t, 1, apps)
}
func TestPostgresOrderDuplicateStaleAndRestart(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 2, nil)
	a := newService(t, "alpha", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	b := newService(t, "beta", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "tv_show") })
	register(t, s, b)
	register(t, s, a)
	_, e := s.Scan(testContext)
	require.NoError(t, e)
	l, e := s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, l)
	require.Equal(t, "alpha", l.Plan.Steps[0].Entry.ID)
	j, e := s.Prepare(testContext, *l)
	require.NoError(t, e)
	require.NoError(t, s.startAttempt(testContext, *l))
	r := result(j, "movie")
	require.NoError(t, s.Finish(testContext, *l, j, r, true))
	require.ErrorIs(t, s.Finish(testContext, *l, j, r, true), ErrStale)
	run(t, s)
	var kind string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind))
	require.Equal(t, "movie", kind)
	seedTorrent(t, p, 3, nil)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	l, e = s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, l)
	j, e = s.Prepare(testContext, *l)
	require.NoError(t, e)
	require.NoError(t, s.startAttempt(testContext, *l))
	_, e = p.Exec(testContext, `UPDATE processor_jobs SET lease_until=now()-interval '1 second' WHERE job_id=$1`, l.ID)
	require.NoError(t, e)
	restarted, e := NewStore(s.cfg, s.pool, s.policy)
	require.NoError(t, e)
	again, e := restarted.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, again)
	require.Greater(t, again.Generation, l.Generation)
	same, e := restarted.Prepare(testContext, *again)
	require.NoError(t, e)
	require.Equal(t, j.Binding, same.Binding)
	require.ErrorIs(t, s.Finish(testContext, *l, j, result(j, "movie"), true), ErrStale)
	_, e = p.Exec(testContext, `UPDATE torrents SET name='Changed metadata' WHERE info_hash=$1`, again.Hash)
	require.NoError(t, e)
	require.NoError(t, restarted.Finish(testContext, *again, same, result(same, "movie"), false)) // step 0 recorded, final application rechecks again
	_, e = restarted.RunOne(testContext)
	require.ErrorIs(t, e, ErrStale) // stale source is refused before egress
}
func TestPostgresFailuresAreBoundedAndNeverJunk(t *testing.T) {
	for _, behavior := range []string{"timeout", "malformed", "abstain", "defer", "crash"} {
		t.Run(behavior, func(t *testing.T) {
			s, p, _ := fixture(t)
			h := seedTorrent(t, p, 4, "movie")
			a := newService(t, "fault", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result {
				switch behavior {
				case "timeout":
					time.Sleep(250 * time.Millisecond)
				case "malformed":
					r := result(j, "movie")
					r.JobID = strings.Repeat("0", 64)
					return r
				case "abstain":
					return v1.Result{Binding: j.Binding, Outcome: v1.Abstained}
				case "defer":
					return v1.Result{Binding: j.Binding, Outcome: v1.Deferred}
				}
				return result(j, "movie")
			})
			register(t, s, a)
			if behavior == "crash" {
				a.server.Close()
			}
			_, e := s.Scan(testContext)
			require.NoError(t, e)
			run(t, s)
			_, e = p.Exec(testContext, `UPDATE processor_jobs SET available_at=now() WHERE state='deferred'`)
			require.NoError(t, e)
			if behavior != "abstain" {
				run(t, s)
			}
			var attempts int
			var state, outcome string
			require.NoError(t, p.QueryRow(testContext, `SELECT attempts,state,outcome FROM processor_jobs`).Scan(&attempts, &state, &outcome))
			require.LessOrEqual(t, attempts, 2)
			require.Equal(t, "completed", state)
			if behavior == "abstain" {
				require.Equal(t, "abstained", outcome)
			} else if behavior == "defer" {
				require.Equal(t, "deferred", outcome)
			} else {
				require.Equal(t, "failed", outcome)
			}
			var kept int
			require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM torrent_contents WHERE info_hash=$1 AND content_type='movie'`, h).Scan(&kept))
			require.Equal(t, 1, kept)
			require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_exclusions`).Scan(&kept))
			require.Zero(t, kept)
		})
	}
}
func TestPostgresUpgradeRollbackCancellationAndBackpressure(t *testing.T) {
	s, p, _ := fixture(t)
	seedTorrent(t, p, 5, nil)
	seedTorrent(t, p, 6, nil)
	s.cfg.QueueCapacity = 1
	a := newService(t, "versioned", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	old := register(t, s, a)
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n)
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	l, e := s.Claim(testContext)
	require.NoError(t, e)
	j, e := s.Prepare(testContext, *l)
	require.NoError(t, e)
	b := newService(t, "versioned", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "tv_show") })
	b.identity.Version = "2.0.0"
	w := b.write()
	w.ExpectedGeneration = old.Generation
	bad := b.identity
	bad.Version = "wrong"
	w.ExpectedProcessor = &bad
	_, e = s.Register(testContext, b.identity.ID, w)
	require.Error(t, e)
	w.ExpectedProcessor = &b.identity
	newer, e := s.Register(testContext, b.identity.ID, w)
	require.NoError(t, e)
	require.ErrorIs(t, s.Finish(testContext, *l, j, result(j, "movie"), false), ErrStale)
	_, e = s.Register(testContext, b.identity.ID, w)
	require.ErrorIs(t, e, ErrConflict)
	w = a.write()
	w.ExpectedGeneration = newer.Generation
	w.ExpectedProcessor = &a.identity
	rolled, e := s.Register(testContext, a.identity.ID, w)
	require.NoError(t, e)
	require.Greater(t, rolled.ExecutionGeneration, newer.ExecutionGeneration)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	l, e = s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, l)
	state, e := s.Cancel(testContext, l.ID)
	require.NoError(t, e)
	require.Equal(t, "canceled", state)
	state, e = s.Cancel(testContext, l.ID)
	require.NoError(t, e)
	require.Equal(t, "canceled", state)
	require.NoError(t, s.Remove(testContext, a.identity.ID, rolled.Generation))
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
}

// Keep generated model codecs exercised in the source snapshot path.
var _ = model.Torrent{}
