package cataloguerecovery_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/blocking"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery/recoveryhttp"
	"github.com/spencercnorton/bitagent/internal/database/dao"
	"github.com/spencercnorton/bitagent/internal/database/search"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protocol"
	"github.com/spencercnorton/bitagent/internal/torznab"
	"github.com/spencercnorton/bitagent/internal/torznab/adapter"
	torznabhttp "github.com/spencercnorton/bitagent/internal/torznab/httpserver"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var ctx = context.Background()
var fixtureTables = []string{"torrents", "torrent_sources", "torrents_torrent_sources", "torrent_files", "torrent_pieces", "torrent_hints", "torrent_tags", "torrent_contents", "torrent_tracker_seeds", "junkpurge_sync_claims"}

func recoveryPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN for disposable PostgreSQL recovery drills")
	}
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("recovery_test_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `create schema `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	cfg.ConnConfig.RuntimeParams["application_name"] = schema
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	t.Cleanup(func() {
		pool.Close()
		admin, e := pgx.Connect(ctx, dsn)
		if e == nil {
			_, _ = admin.Exec(ctx, `drop schema `+pgx.Identifier{schema}.Sanitize()+` cascade`)
			_ = admin.Close(ctx)
		}
	})
	return pool
}
func cfg() cataloguerecovery.Config {
	return cataloguerecovery.Config{Enabled: true, Retention: 24 * time.Hour, MaxPayloadBytes: 64 << 20, MaxSnapshotBytes: 8 << 20, MaxSnapshots: 128, MaxStorageBytes: 128 << 20, MaxRowsPerSnapshot: 1024}
}
func store(t *testing.T, pool *pgxpool.Pool) *cataloguerecovery.Store {
	t.Helper()
	s, e := cataloguerecovery.NewStore(pool, cfg())
	require.NoError(t, e)
	return s
}
func hash(n byte) []byte { h := make([]byte, 20); h[19] = n; return h }
func seed(t *testing.T, pool *pgxpool.Pool, h []byte, single bool) {
	t.Helper()
	status := "multi"
	files := any(2)
	if single {
		status = "single"
		files = nil
	}
	_, err := pool.Exec(ctx, `insert into torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) values($1,'SyntheticRecovery.2031.ENG.mkv',4096,false,$2,$3,now()-interval '2 days',now()-interval '1 day');
 insert into torrent_sources(key,name,created_at,updated_at) values('synthetic_source','Synthetic source',now()-interval '5 days',now()-interval '4 days') on conflict do nothing;
 insert into torrents_torrent_sources(source,info_hash,import_id,seeders,leechers,published_at,created_at,updated_at) values('synthetic_source',$1,'synthetic-import',null,3,now()-interval '3 days',now()-interval '2 days',now()-interval '1 day'),('dht',$1,null,2,null,null,now()-interval '2 days',now()-interval '1 day');
 insert into torrent_pieces(info_hash,piece_length,pieces,created_at) values($1,16384,decode('00ff1234','hex'),now()-interval '2 days');
 insert into torrent_hints(info_hash,content_type,title,release_year,languages,episodes,video_resolution,video_source,video_codec,release_group,created_at,updated_at) values($1,'movie','SyntheticRecovery',2031,'["en"]','{}','V1080p','WEBDL','H264','SYN',now()-interval '2 days',now()-interval '1 day');
 insert into torrent_tags(info_hash,name,created_at,updated_at) values($1,'ordinary',now()-interval '2 days',now()-interval '1 day');
 insert into content(type,source,id,title,release_year,created_at,updated_at) values('movie','tmdb','991001','SyntheticRecovery',2031,now()-interval '4 days',now()-interval '1 day') on conflict do nothing;
 insert into torrent_contents(info_hash,content_type,content_source,content_id,video_resolution,video_source,video_codec,release_group,size,files_count,created_at,updated_at) values($1,'movie','tmdb','991001','V1080p','WEBDL','H264','SYN',4096,$3,now()-interval '2 days',now()-interval '1 day');
 insert into torrent_tracker_seeds(info_hash,tracker_known,seeders,leechers,completed,best_tracker,checked_at,created_at,prev_seeders,peak_seeders,last_positive_at) values($1,true,2,null,13,'udp://synthetic.invalid',now(),now()-interval '2 days',1,8,now()-interval '1 hour');
 insert into junkpurge_sync_claims(info_hash,owner,torrent_name,claimed_at,lease_until) values($1,'synthetic-expired','SyntheticRecovery',now()-interval '2 days',now()-interval '1 day');
 update torrent_contents set tsv=to_tsvector('simple','SyntheticRecovery ' || encode($1::bytea,'hex')) where info_hash=$1;
 insert into label_evidence(source,source_kind,source_instance,source_object_id,info_hash,observed_at,strength,raw_payload) values('synthetic','historic','fixture',encode($1::bytea,'hex'),$1,now()-interval '2 days',1,'{"historic":true}');`, h, status, files)
	require.NoError(t, err)
	if !single {
		_, err = pool.Exec(ctx, `insert into torrent_files(info_hash,"index",path,size,created_at,updated_at) values($1,0,'SyntheticRecovery/Part.1.ENG.mkv',2048,now()-interval '2 days',now()-interval '1 day'),($1,1,'SyntheticRecovery/Part.2.ENG.mkv',2048,now()-interval '2 days',now()-interval '1 day')`, h)
		require.NoError(t, err)
	}
}
func raw(t *testing.T, pool *pgxpool.Pool, h []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range fixtureTables {
		where := `info_hash=$1`
		if table == "torrent_sources" {
			where = `key in(select source from torrents_torrent_sources where info_hash=$1)`
		}
		var b string
		err := pool.QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(r) order by to_jsonb(r)::text),'[]'::jsonb)::text from `+pgx.Identifier{table}.Sanitize()+` r where `+where, h).Scan(&b)
		require.NoError(t, err)
		out[table] = b
	}
	return out
}
func count(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+pgx.Identifier{table}.Sanitize()).Scan(&n))
	return n
}
func assertRetained(t *testing.T, pool *pgxpool.Pool, id int64, state string) {
	t.Helper()
	var got string
	require.NoError(t, pool.QueryRow(ctx, `select state from catalogue_recovery_snapshots where id=$1`, id).Scan(&got))
	require.Equal(t, state, got)
}
func TestPostgresFullSourceRoundtripAndIdempotency(t *testing.T) {
	for _, single := range []bool{false, true} {
		t.Run(fmt.Sprint(single), func(t *testing.T) {
			pool := recoveryPool(t)
			s := store(t, pool)
			h := hash(1)
			seed(t, pool, h, single)
			before := raw(t, pool, h)
			now := time.Now().UTC()
			snap, e := s.Remove(ctx, h, "synthetic removal", now)
			require.NoError(t, e)
			require.Zero(t, count(t, pool, "torrents"))
			require.Equal(t, 1, count(t, pool, "label_evidence"))
			require.Equal(t, 1, count(t, pool, "content"))
			retry, e := s.Remove(ctx, h, "synthetic removal", now)
			require.NoError(t, e)
			require.Equal(t, snap.ID, retry.ID)
			require.Equal(t, 1, count(t, pool, "catalogue_recovery_snapshots"))
			require.Equal(t, 2, count(t, pool, "catalogue_recovery_events"))
			ok, e := s.Restore(ctx, snap.ID, now.Add(time.Minute))
			require.NoError(t, e)
			require.True(t, ok)
			require.Equal(t, before, raw(t, pool, h))
			ok, e = s.Restore(ctx, snap.ID, now.Add(2*time.Minute))
			require.NoError(t, e)
			require.False(t, ok)
			require.Equal(t, 3, count(t, pool, "catalogue_recovery_events"))
			require.Equal(t, 2, count(t, pool, "torrent_verdict_events"))
			assertRetained(t, pool, snap.ID, "restored")
		})
	}
}
func TestPostgresRecoveryTransitionRollback(t *testing.T) {
	for _, failure := range []string{"prepared", "removed", "restored"} {
		t.Run(failure, func(t *testing.T) {
			pool := recoveryPool(t)
			s := store(t, pool)
			h := hash(2)
			seed(t, pool, h, false)
			before := raw(t, pool, h)
			var snap cataloguerecovery.Snapshot
			var err error
			if failure == "restored" {
				snap, err = s.Remove(ctx, h, "synthetic", time.Now())
				require.NoError(t, err)
			}
			_, err = pool.Exec(ctx, `alter table catalogue_recovery_events add constraint synthetic_failure check(transition<>$1)`, failure)
			require.NoError(t, err)
			if failure == "restored" {
				ok, e := s.Restore(ctx, snap.ID, time.Now())
				require.Error(t, e)
				require.False(t, ok)
				require.Zero(t, count(t, pool, "torrents"))
				assertRetained(t, pool, snap.ID, "removed")
				require.Equal(t, 1, count(t, pool, "torrent_verdict_events"))
			} else {
				_, e := s.Remove(ctx, h, "synthetic", time.Now())
				require.Error(t, e)
				require.Equal(t, before, raw(t, pool, h))
				require.Zero(t, count(t, pool, "catalogue_recovery_snapshots"))
				require.Zero(t, count(t, pool, "torrent_verdict_events"))
			}
			_, err = pool.Exec(ctx, `alter table catalogue_recovery_events drop constraint synthetic_failure`)
			require.NoError(t, err)
			if failure == "restored" {
				ok, e := s.Restore(ctx, snap.ID, time.Now())
				require.NoError(t, e)
				require.True(t, ok)
			} else {
				_, e := s.Remove(ctx, h, "synthetic", time.Now())
				require.NoError(t, e)
			}
		})
	}
}
func TestPostgresRecoveryFailClosedBounds(t *testing.T) {
	for _, cap := range []string{"rows", "snapshot", "payload", "count", "storage"} {
		t.Run(cap, func(t *testing.T) {
			pool := recoveryPool(t)
			h := hash(3)
			seed(t, pool, h, false)
			c := cfg()
			switch cap {
			case "rows":
				c.MaxRowsPerSnapshot = 1
			case "snapshot":
				c.MaxSnapshotBytes = 128
			case "payload":
				c.MaxPayloadBytes = 128
				c.MaxSnapshotBytes = 128
			case "count":
				c.MaxSnapshots = 1
			case "storage":
				c.MaxStorageBytes = 1
			}
			s, e := cataloguerecovery.NewStore(pool, c)
			require.NoError(t, e)
			if cap == "count" {
				_, e = s.Remove(ctx, h, "synthetic", time.Now())
				require.NoError(t, e)
				h = hash(4)
				seed(t, pool, h, false)
			}
			before := raw(t, pool, h)
			_, e = s.Remove(ctx, h, "synthetic", time.Now())
			require.ErrorIs(t, e, cataloguerecovery.ErrCapacity)
			require.Equal(t, before, raw(t, pool, h))
		})
	}
}
func TestPostgresRecoveryConflictsAndRetention(t *testing.T) {
	for _, conflict := range []string{"recrawl", "source", "schema", "payload", "dependency", "verdict", "quarantine", "canonical", "expiry", "new_cascade"} {
		t.Run(conflict, func(t *testing.T) {
			pool := recoveryPool(t)
			s := store(t, pool)
			h := hash(5)
			seed(t, pool, h, false)
			now := time.Now()
			snap, e := s.Remove(ctx, h, "synthetic", now)
			require.NoError(t, e)
			var q string
			switch conflict {
			case "recrawl":
				q = `insert into torrents(info_hash,name,size,private,created_at,updated_at) values($1,'NewCurrentSource',5,false,now(),now())`
			case "source":
				q = `update torrent_sources set name='Changed source' where key='synthetic_source'`
			case "schema":
				q = `alter table torrent_files add column extra text`
			case "payload":
				q = `update catalogue_recovery_snapshots set snapshot=jsonb_set(snapshot,'{torrents,0,name}','"Tampered"') where info_hash=$1`
			case "dependency":
				q = `delete from content where id='991001'`
			case "verdict":
				q = `insert into torrent_verdict_state(info_hash,verdict,mechanism,since) values($1,'blacklisted','csam',now()) on conflict(info_hash) do update set verdict='blacklisted',mechanism='csam';insert into torrent_verdict_events(info_hash,verdict,mechanism,reason) values($1,'blacklisted','csam','Independent block')`
			case "quarantine":
				q = `insert into junkpurge_quarantine(info_hash,torrent_name,verdict,confidence,torrent_snapshot) values($1,'Synthetic','junk',0.99,'{}')`
			case "canonical":
				q = `insert into torrent_canonical_labels(info_hash,resolved_source,resolved_strength,resolved_at) values($1,'synthetic',10,now())`
			case "expiry":
				now = now.Add(25 * time.Hour)
			case "new_cascade":
				q = `create table synthetic_dependency(info_hash bytea references torrents on delete cascade)`
			}
			if q != "" {
				if strings.Contains(q, "$1") {
					_, e = pool.Exec(ctx, q, h)
				} else {
					_, e = pool.Exec(ctx, q)
				}
				require.NoError(t, e)
			}
			ok, e := s.Restore(ctx, snap.ID, now)
			require.Error(t, e)
			require.False(t, ok)
			assertRetained(t, pool, snap.ID, "removed")
			if conflict == "recrawl" {
				require.Equal(t, 1, count(t, pool, "torrents"))
			} else {
				require.Zero(t, count(t, pool, "torrents"))
			}
			require.Equal(t, 2, count(t, pool, "catalogue_recovery_events"))
		})
	}
}
func TestPostgresRecoveryProtectsCurrentState(t *testing.T) {
	for _, protected := range []string{"private", "canonical", "hint", "tag", "lease", "quarantine", "verdict", "unhandled_cascade"} {
		t.Run(protected, func(t *testing.T) {
			pool := recoveryPool(t)
			s := store(t, pool)
			h := hash(6)
			seed(t, pool, h, false)
			var q string
			switch protected {
			case "private":
				q = `update torrents set private=true where info_hash=$1`
			case "canonical":
				q = `insert into torrent_canonical_labels(info_hash,resolved_source,resolved_strength,resolved_at) values($1,'synthetic',10,now())`
			case "hint":
				q = `update torrent_hints set content_source='tmdb',content_id='991001' where info_hash=$1`
			case "tag":
				q = `insert into torrent_tags(info_hash,name,created_at,updated_at) values($1,'wanted',now(),now())`
			case "lease":
				q = `update junkpurge_sync_claims set lease_until=now()+interval '1 hour' where info_hash=$1`
			case "quarantine":
				q = `insert into junkpurge_quarantine(info_hash,torrent_name,verdict,confidence,torrent_snapshot) values($1,'Synthetic','junk',0.99,'{}')`
			case "verdict":
				q = `insert into torrent_verdict_state(info_hash,verdict,mechanism,since) values($1,'blacklisted','csam',now())`
			case "unhandled_cascade":
				q = `create table synthetic_dependency(info_hash bytea references torrents on delete cascade)`
			}
			var e error
			if strings.Contains(q, "$1") {
				_, e = pool.Exec(ctx, q, h)
			} else {
				_, e = pool.Exec(ctx, q)
			}
			require.NoError(t, e)
			before := raw(t, pool, h)
			_, e = s.Remove(ctx, h, "synthetic", time.Now())
			require.Error(t, e)
			require.Equal(t, before, raw(t, pool, h))
			require.Zero(t, count(t, pool, "catalogue_recovery_snapshots"))
		})
	}
}
func isSerialization(err error) bool {
	var pg interface{ SQLState() string }
	return errors.As(err, &pg) && pg.SQLState() == "40001"
}
func TestPostgresConcurrentCapacityAndRestore(t *testing.T) {
	pool := recoveryPool(t)
	h1, h2 := hash(7), hash(8)
	seed(t, pool, h1, false)
	seed(t, pool, h2, false)
	c := cfg()
	c.MaxSnapshots = 1
	s, e := cataloguerecovery.NewStore(pool, c)
	require.NoError(t, e)
	errs := make(chan error, 2)
	snaps := make(chan cataloguerecovery.Snapshot, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, h := range [][]byte{h1, h2} {
		wg.Add(1)
		go func(h []byte) {
			defer wg.Done()
			<-start
			snap, err := s.Remove(ctx, h, "synthetic", time.Now())
			errs <- err
			snaps <- snap
		}(h)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(snaps)
	n := 0
	var chosen cataloguerecovery.Snapshot
	for err := range errs {
		if err == nil {
			n++
		} else {
			require.True(t, isSerialization(err) || err == cataloguerecovery.ErrCapacity, "%v", err)
		}
	}
	require.Equal(t, 1, n)
	for snap := range snaps {
		if snap.ID > 0 {
			chosen = snap
		}
	}
	require.Equal(t, 1, count(t, pool, "catalogue_recovery_snapshots"))
	require.Equal(t, 1, count(t, pool, "torrents"))
	start = make(chan struct{})
	results := make(chan bool, 8)
	errs = make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.Restore(ctx, chosen.ID, time.Now())
			results <- ok
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	n = 0
	for ok := range results {
		if ok {
			n++
		}
	}
	require.Equal(t, 1, n)
	for err := range errs {
		if err != nil {
			require.True(t, isSerialization(err), "%v", err)
		}
	}
	require.Equal(t, 2, count(t, pool, "torrents"))
	require.Equal(t, 3, count(t, pool, "catalogue_recovery_events"))
}
func nativeManager(t *testing.T, pool *pgxpool.Pool, s *cataloguerecovery.Store) blocking.Manager {
	t.Helper()
	lp := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	base, e := blocking.New(blocking.Params{Pool: lp, PgxPoolWait: &sync.WaitGroup{}}).Manager.Get()
	require.NoError(t, e)
	m, e := blocking.WithRecovery(base, s)
	require.NoError(t, e)
	return m
}
func TestPostgresHTTPRestoreActuallyReleasesBoundCrawlerBloom(t *testing.T) {
	pool := recoveryPool(t)
	s := store(t, pool)
	h := hash(9)
	id, e := protocol.NewIDFromByteSlice(h)
	require.NoError(t, e)
	seed(t, pool, h, false)
	before := raw(t, pool, h)
	m := nativeManager(t, pool, s)
	require.NoError(t, m.Block(ctx, []protocol.ID{id}, true))
	kept, e := m.Filter(ctx, []protocol.ID{id})
	require.NoError(t, e)
	require.Empty(t, kept)
	var snap int64
	require.NoError(t, pool.QueryRow(ctx, `select id from catalogue_recovery_snapshots where info_hash=$1`, h).Scan(&snap))
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	t.Cleanup(func() { _ = db.Close() })
	gdb, e := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, e)
	dq := dao.Use(gdb)
	searcher, e := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return dq, nil })}).Search.Get()
	require.NoError(t, e)
	vs := verdicts.NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	client := adapter.NewWithFilters(searcher, nil, false, nil, adapter.FreshnessConfigFromTorznab(torznab.NewDefaultConfig())).WithVerdicts(vs, true, nil)
	engine := gin.New()
	require.NoError(t, recoveryhttp.New(s, func(c *gin.Context) {
		if c.GetHeader("X-Operator-Test") != "synthetic" {
			c.AbortWithStatus(http.StatusUnauthorized)
		}
	}).Apply(engine))
	require.NoError(t, torznabhttp.New(lazy.New(func() (torznab.Client, error) { return client, nil }), torznab.NewDefaultConfig(), nil).Apply(engine))
	request := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Operator-Test", "synthetic")
		engine.ServeHTTP(w, req)
		return w
	}
	unauthorized := httptest.NewRecorder()
	engine.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/catalogue-recovery/%d/restore", snap), nil))
	require.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	require.Zero(t, count(t, pool, "torrents"))
	r := request(http.MethodPost, fmt.Sprintf("/catalogue-recovery/%d/restore", snap))
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &body))
	require.Equal(t, true, body["restored"])
	require.Equal(t, before, raw(t, pool, h))
	kept, e = m.Filter(ctx, []protocol.ID{id})
	require.NoError(t, e)
	require.Equal(t, []protocol.ID{id}, kept)
	// Reload the persisted bloom in another native manager: the exception is
	// durable, while a subsequent block supersedes only its own restoration.
	restart := nativeManager(t, pool, s)
	kept, e = restart.Filter(ctx, []protocol.ID{id})
	require.NoError(t, e)
	require.Equal(t, []protocol.ID{id}, kept)
	r = request(http.MethodGet, "/torznab/api?t=search&q=SyntheticRecovery")
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Contains(t, r.Body.String(), "SyntheticRecovery.2031.ENG.mkv")
	require.NoError(t, restart.Block(ctx, []protocol.ID{id}, true))
	kept, e = m.Filter(ctx, []protocol.ID{id})
	require.NoError(t, e)
	require.Empty(t, kept)
	r = request(http.MethodPost, fmt.Sprintf("/catalogue-recovery/%d/restore", snap))
	require.Equal(t, http.StatusOK, r.Code)
	kept, e = m.Filter(ctx, []protocol.ID{id})
	require.NoError(t, e)
	require.Empty(t, kept, "idempotent old restore cannot release a later block")
}
func TestPostgresRestoreBeforePendingBloomFlush(t *testing.T) {
	pool := recoveryPool(t)
	s := store(t, pool)
	h := hash(10)
	id, e := protocol.NewIDFromByteSlice(h)
	require.NoError(t, e)
	seed(t, pool, h, true)
	m := nativeManager(t, pool, s)
	require.NoError(t, m.Block(ctx, []protocol.ID{id}, false))
	var snap int64
	require.NoError(t, pool.QueryRow(ctx, `select id from catalogue_recovery_snapshots where info_hash=$1`, h).Scan(&snap))
	ok, e := s.Restore(ctx, snap, time.Now())
	require.NoError(t, e)
	require.True(t, ok)
	require.NoError(t, m.Flush(ctx))
	kept, e := m.Filter(ctx, []protocol.ID{id})
	require.NoError(t, e)
	require.Equal(t, []protocol.ID{id}, kept)
	require.Equal(t, 1, count(t, pool, "torrents"))
}
func TestPostgresBackendTerminationRollsBackWholeRemoval(t *testing.T) {
	pool := recoveryPool(t)
	s := store(t, pool)
	h := hash(11)
	seed(t, pool, h, false)
	before := raw(t, pool, h)
	_, e := pool.Exec(ctx, `create function synthetic_crash_barrier() returns trigger language plpgsql as $$ begin if new.transition='removed' then perform pg_advisory_xact_lock(9159,3); end if; return new; end $$;create trigger synthetic_crash before insert on catalogue_recovery_events for each row execute function synthetic_crash_barrier()`)
	require.NoError(t, e)
	barrier, e := pool.Acquire(ctx)
	require.NoError(t, e)
	defer barrier.Release()
	_, e = barrier.Exec(ctx, `select pg_advisory_lock(9159,3)`)
	require.NoError(t, e)
	defer barrier.Exec(ctx, `select pg_advisory_unlock(9159,3)`) //nolint:errcheck
	done := make(chan error, 1)
	go func() { _, err := s.Remove(ctx, h, "synthetic", time.Now()); done <- err }()
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e = barrier.QueryRow(ctx, `select a.pid from pg_stat_activity a join pg_locks l on l.pid=a.pid where a.application_name=$1 and l.locktype='advisory' and l.classid=9159 and l.objid=3 and not l.granted limit 1`, pool.Config().ConnConfig.RuntimeParams["application_name"]).Scan(&pid)
		if e == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Positive(t, pid)
	var killed bool
	require.NoError(t, barrier.QueryRow(ctx, `select pg_terminate_backend($1)`, pid).Scan(&killed))
	require.True(t, killed)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("removal did not fail after backend termination")
	}
	require.Equal(t, before, raw(t, pool, h))
	require.Zero(t, count(t, pool, "catalogue_recovery_snapshots"))
	require.Zero(t, count(t, pool, "torrent_verdict_events"))
	_, e = pool.Exec(ctx, `drop trigger synthetic_crash on catalogue_recovery_events`)
	require.NoError(t, e)
	_, e = s.Remove(ctx, h, "synthetic", time.Now())
	require.NoError(t, e)
}

func TestPostgresConcurrentDuplicateRemovalAndAtomicBatch(t *testing.T) {
	pool := recoveryPool(t)
	s := store(t, pool)
	h := hash(12)
	seed(t, pool, h, false)
	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, e := s.Remove(ctx, h, "synthetic", time.Now()); errs <- e }()
	}
	close(start)
	wg.Wait()
	close(errs)
	success := 0
	for e := range errs {
		if e == nil {
			success++
		} else {
			require.True(t, isSerialization(e), "%v", e)
		}
	}
	require.Positive(t, success)
	require.Equal(t, 1, count(t, pool, "catalogue_recovery_snapshots"))
	require.Equal(t, 2, count(t, pool, "catalogue_recovery_events"))
	require.Equal(t, 1, count(t, pool, "torrent_verdict_events"))
	h2, h3 := hash(13), hash(14)
	seed(t, pool, h2, false)
	seed(t, pool, h3, false)
	_, e := pool.Exec(ctx, `insert into torrent_tags(info_hash,name,created_at,updated_at) values($1,'wanted',now(),now())`, h3)
	require.NoError(t, e)
	_, e = s.RemoveBatch(ctx, [][]byte{h2, h3}, "synthetic", time.Now())
	require.ErrorIs(t, e, cataloguerecovery.ErrProtected)
	require.Equal(t, 2, count(t, pool, "torrents"))
	require.Equal(t, 1, count(t, pool, "catalogue_recovery_snapshots"))
	require.Equal(t, 1, count(t, pool, "torrent_verdict_events"))
	_, e = s.RemoveBatch(ctx, [][]byte{h2, h2}, "synthetic", time.Now())
	require.Error(t, e)
	_, e = s.Remove(ctx, hash(99), "synthetic", time.Now())
	require.ErrorIs(t, e, cataloguerecovery.ErrConflict, "an unsnapshotted historical loss cannot be recovered")
}

func TestPostgresRestoreRetainsReleaseClaimsAndIndependentHistories(t *testing.T) {
	pool := recoveryPool(t)
	s := store(t, pool)
	h := hash(91)
	seed(t, pool, h, false)
	name := "SyntheticRecovery.2031.2160p.HEVC.DV.HDR10.DDP5.1.Atmos.REPACK.x265.ENG.mkv"
	attrs := model.InferReleaseAttributes(name, "HEVC.DV.HDR10.DDP5.1.Atmos.REPACK.x265.ENG")
	require.NotNil(t, attrs)
	require.NotEmpty(t, attrs.HDRFormats)
	require.NotEmpty(t, attrs.AudioFormats)
	require.NotEmpty(t, attrs.AudioFeatures)
	require.NotEmpty(t, attrs.Revisions)
	require.NotNil(t, attrs.AudioChannels)
	require.NotNil(t, attrs.Encoder)
	body, err := json.Marshal(attrs)
	require.NoError(t, err)
	// Synthetic history checks retention only. These digest-only fixtures
	// cannot qualify provider replay, committed application or human labels.
	_, err = pool.Exec(ctx, `update torrents set name=$2 where info_hash=$1;
update torrent_contents set release_attributes=$3::jsonb where info_hash=$1;
insert into llm_work_tasks(task_key,kind,info_hash,source_digest,policy_digest,input_digest,family_digest,payload,priority,time_bucket,daily_limit,monthly_limit,state,expires_at,completed_at)
values(decode(repeat('11',32),'hex'),'classifier_type',$1,decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),decode(repeat('44',32),'hex'),decode(repeat('55',32),'hex'),'{}',50,0,0,0,'completed',now()+interval '1 day',now());
insert into llm_work_events(task_key,state,reason) values(decode(repeat('11',32),'hex'),'completed','synthetic');
insert into llm_work_applications(task_key,info_hash,source_digest,policy_digest,applied_snapshot)
values(decode(repeat('11',32),'hex'),$1,decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),'{"synthetic_history":true}');
insert into llm_capture_dispatch_attempts(capture_key,semantic_key,task,task_key,state,reason)
values(decode(repeat('66',32),'hex'),decode(repeat('77',32),'hex'),'classifier_type',decode(repeat('11',32),'hex'),'result','synthetic');
insert into llm_capture_dispatch_aliases(capture_key,fence_key) values(decode(repeat('88',32),'hex'),decode(repeat('66',32),'hex'));
insert into release_field_repair_journal(plan_digest,target_id,info_hash,source_name_sha256,before_fields,after_fields,application_before,application_after,applied_updated_at)
values(decode(repeat('99',32),'hex'),'synthetic-history',$1,$4,'{}','{}','{}','{}',now());`, h, name, string(body), attrs.SourceNameSHA256)
	require.NoError(t, err)
	histories := func() map[string]string {
		out := map[string]string{}
		for _, table := range []string{"llm_work_tasks", "llm_work_events", "llm_work_applications", "llm_capture_dispatch_attempts", "llm_capture_dispatch_aliases", "release_field_repair_journal"} {
			var data string
			require.NoError(t, pool.QueryRow(ctx, `select jsonb_agg(to_jsonb(r) order by to_jsonb(r)::text)::text from `+pgx.Identifier{table}.Sanitize()+` r`).Scan(&data))
			out[table] = data
		}
		return out
	}
	before, retained := raw(t, pool, h), histories()
	now := time.Now().UTC()
	snap, err := s.Remove(ctx, h, "synthetic compatibility removal", now)
	require.NoError(t, err)
	require.Zero(t, count(t, pool, "torrent_contents"))
	require.Equal(t, retained, histories(), "independent task/dispatch/application/repair history must survive raw cascade")
	restored, err := s.Restore(ctx, snap.ID, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, restored)
	require.Equal(t, before, raw(t, pool, h), "all non-NULL release claims and all raw source fields must roundtrip exactly")
	require.Equal(t, retained, histories(), "restore cannot rewrite or refund permanent histories")
}

func recoveryMigrationProvider(t *testing.T, pool *pgxpool.Pool) *goose.Provider {
	t.Helper()
	db := stdlib.OpenDB(*pool.Config().ConnConfig)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	return provider
}
func recoveryHistory(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"catalogue_recovery_budget", "catalogue_recovery_snapshots", "catalogue_recovery_events", "torrent_verdict_events", "torrent_verdict_state", "goose_db_version"} {
		var data string
		require.NoError(t, pool.QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(r) order by to_jsonb(r)::text),'[]'::jsonb)::text from `+pgx.Identifier{table}.Sanitize()+` r`).Scan(&data))
		out[table] = data
	}
	return out
}
func TestPostgresRecoveryDowngradeRetainsRemovedRestoredExpiredAndBudgets(t *testing.T) {
	for _, state := range []string{"removed", "restored", "expired", "snapshot_budget_only", "payload_budget_only"} {
		t.Run(state, func(t *testing.T) {
			pool := recoveryPool(t)
			provider := recoveryMigrationProvider(t, pool)
			h := hash(92)
			seed(t, pool, h, false)
			now := time.Now().UTC()
			if strings.HasSuffix(state, "budget_only") {
				column := "snapshots"
				if state == "payload_budget_only" {
					column = "payload_bytes"
				}
				_, err := pool.Exec(ctx, `update catalogue_recovery_budget set `+column+`=1`)
				require.NoError(t, err)
			} else {
				c := cfg()
				if state == "expired" {
					c.Retention = time.Millisecond
				}
				s, err := cataloguerecovery.NewStore(pool, c)
				require.NoError(t, err)
				snap, err := s.Remove(ctx, h, "synthetic downgrade guard", now)
				require.NoError(t, err)
				if state == "restored" {
					ok, e := s.Restore(ctx, snap.ID, now.Add(time.Second))
					require.NoError(t, e)
					require.True(t, ok)
				}
				if state == "expired" {
					time.Sleep(2 * time.Millisecond)
					ok, e := s.Restore(ctx, snap.ID, time.Now())
					require.ErrorIs(t, e, cataloguerecovery.ErrExpired)
					require.False(t, ok)
					var expired bool
					require.NoError(t, pool.QueryRow(ctx, `select expires_at < clock_timestamp() from catalogue_recovery_snapshots where id=$1`, snap.ID).Scan(&expired))
					require.True(t, expired)
				}
			}
			before, hist := raw(t, pool, h), recoveryHistory(t, pool)
			_, err := provider.DownTo(ctx, 58)
			require.ErrorContains(t, err, "catalogue recovery history and budgets must be retained")
			require.Equal(t, before, raw(t, pool, h))
			require.Equal(t, hist, recoveryHistory(t, pool), "rejected downgrade cannot mutate history, budgets, verdicts or migration receipt")
		})
	}
}
func TestPostgresRecoveryEmptyDowngradeAndRetry(t *testing.T) {
	pool := recoveryPool(t)
	provider := recoveryMigrationProvider(t, pool)
	h := hash(93)
	seed(t, pool, h, true)
	before := raw(t, pool, h)
	down, err := provider.DownTo(ctx, 58)
	require.NoError(t, err)
	require.Len(t, down, 1)
	require.Equal(t, before, raw(t, pool, h))
	up, err := provider.UpTo(ctx, 59)
	require.NoError(t, err)
	require.Len(t, up, 1)
	require.Equal(t, before, raw(t, pool, h))
	require.Zero(t, count(t, pool, "catalogue_recovery_snapshots"))
	require.Zero(t, count(t, pool, "catalogue_recovery_events"))
	var used, snapshots int64
	require.NoError(t, pool.QueryRow(ctx, `select payload_bytes,snapshots from catalogue_recovery_budget`).Scan(&used, &snapshots))
	require.Zero(t, used)
	require.Zero(t, snapshots)
}

func TestPostgresRecoveryDowngradeSerializesWithPendingBudgetWriter(t *testing.T) {
	pool := recoveryPool(t)
	provider := recoveryMigrationProvider(t, pool)
	h := hash(94)
	seed(t, pool, h, true)
	before := raw(t, pool, h)
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := pool.Begin(deadline)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(deadline, `update catalogue_recovery_budget set payload_bytes=1`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, e := provider.DownTo(deadline, 58); done <- e }()
	require.Eventually(t, func() bool {
		var blocked bool
		e := pool.QueryRow(deadline, `select exists(select 1 from pg_locks where relation='catalogue_recovery_budget'::regclass and mode='AccessExclusiveLock' and not granted)`).Scan(&blocked)
		return e == nil && blocked
	}, 5*time.Second, 10*time.Millisecond, "downgrade must wait before reading the empty-only guard")
	require.NoError(t, tx.Commit(deadline))
	require.ErrorContains(t, <-done, "catalogue recovery history and budgets must be retained")
	require.Equal(t, before, raw(t, pool, h))
	var used int64
	require.NoError(t, pool.QueryRow(ctx, `select payload_bytes from catalogue_recovery_budget`).Scan(&used))
	require.EqualValues(t, 1, used)
	require.Zero(t, count(t, pool, "catalogue_recovery_snapshots"))
	require.Zero(t, count(t, pool, "catalogue_recovery_events"))
}
