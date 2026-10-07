package cataloguepolicy

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
	"github.com/spencercnorton/bitagent/internal/model"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
)

func policyTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN for disposable PostgreSQL coverage")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("policy_test_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `create schema `+pgx.Identifier{schema}.Sanitize())
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
		conn, e := pgx.Connect(context.Background(), dsn)
		if e == nil {
			_, _ = conn.Exec(context.Background(), `drop schema `+pgx.Identifier{schema}.Sanitize()+` cascade`)
			_ = conn.Close(context.Background())
		}
	})
	_, err = pool.Exec(ctx, `create table torrents(info_hash bytea primary key,name text,updated_at timestamptz,private boolean,files_count integer);
create table torrent_files(info_hash bytea,index int,path text,updated_at timestamptz);
create table torrent_contents(info_hash bytea,is_anime boolean);
create table torrent_canonical_labels(info_hash bytea,category text);
create table torrent_tags(info_hash bytea,name text);
create table torrent_tracker_seeds(info_hash bytea primary key,checked_at timestamptz,tracker_known boolean,seeders int,leechers int,last_positive_at timestamptz);
create table torrents_torrent_sources(info_hash bytea,source text,created_at timestamptz,seeders int,leechers int);
create table label_evidence(id bigserial primary key,info_hash bytea,source text,source_kind text,category text,observed_at timestamptz,raw_payload jsonb);
create table torrent_liveness(info_hash bytea,status text);
`)
	require.NoError(t, err)
	migration, err := migrationssql.FS.ReadFile("00058_catalogue_policy_shadow.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, strings.Split(string(migration), "-- +goose Down")[0])
	require.NoError(t, err)
	return pool
}

func TestPostgresPolicyShadowPersistenceFreshnessPrivacyAndConcurrency(t *testing.T) {
	pool := policyTestPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	cfg := DefaultAvailabilityConfig()
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := make([]byte, 20)
	hash[19] = 1
	_, err := pool.Exec(ctx, `insert into torrents values($1,'Synthetic.Film.2031.1080p.English.Audio',$2,false,1);
insert into torrent_tracker_seeds values($1,$3,true,0,0,null)`, hash, now, now.Add(-6*time.Hour))
	require.NoError(t, err)
	first, err := store.Evaluate(ctx, hash, false, false, now, cfg)
	require.NoError(t, err)
	require.Equal(t, "fresh_known_zero", first.Availability.State)
	var count int
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from catalogue_policy_shadow`).Scan(&count))
	require.Zero(t, count, "dry-run must not persist")
	first, err = store.Evaluate(ctx, hash, true, false, now, cfg)
	require.NoError(t, err)
	require.Equal(t, "eligible_audio", first.English.State)
	_, err = pool.Exec(ctx, `update torrent_tracker_seeds set checked_at=$2 where info_hash=$1`, hash, now.Add(-time.Hour))
	require.NoError(t, err)
	second, err := store.Evaluate(ctx, hash, true, false, now.Add(time.Minute), cfg)
	require.NoError(t, err)
	require.Equal(t, "suspected_unavailable", second.Availability.State)
	require.NotEqual(t, first.InputDigest, second.InputDigest)
	duplicate, err := store.Evaluate(ctx, hash, true, false, now.Add(2*time.Minute), cfg)
	require.NoError(t, err)
	require.Len(t, duplicate.Input.Observations, 2, "same scrape does not multiply observations")
	_, err = pool.Exec(ctx, `update torrent_tracker_seeds set checked_at=$2,seeders=1,last_positive_at=$2 where info_hash=$1`, hash, now)
	require.NoError(t, err)
	revived, err := store.Evaluate(ctx, hash, true, false, now.Add(3*time.Minute), cfg)
	require.NoError(t, err)
	require.Equal(t, "fresh_positive", revived.Availability.State)
	stale, err := store.Evaluate(ctx, hash, true, false, now.Add(25*time.Hour), cfg)
	require.NoError(t, err)
	require.Equal(t, "unknown_stale", stale.Availability.State)
	_, err = store.Evaluate(ctx, hash, true, false, now, cfg)
	require.ErrorContains(t, err, "newer evaluation")
	require.NoError(t, pool.QueryRow(ctx, `select count(*) from torrent_liveness`).Scan(&count))
	require.Zero(t, count, "shadow never writes legacy exclusion")
	_, err = pool.Exec(ctx, `insert into torrent_canonical_labels values($1,'BitGrab')`, hash)
	require.NoError(t, err)
	_, err = store.Evaluate(ctx, hash, true, false, now.Add(26*time.Hour), cfg)
	require.ErrorIs(t, err, ErrPrivate)
	_, err = pool.Exec(ctx, `delete from torrent_canonical_labels where info_hash=$1; update torrents set private=true where info_hash=$1`, hash)
	require.NoError(t, err)
	_, err = store.Evaluate(ctx, hash, false, false, now, cfg)
	require.ErrorIs(t, err, ErrPrivate)
	_, err = pool.Exec(ctx, `update torrents set private=false where info_hash=$1`, hash)
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := store.Evaluate(ctx, hash, true, false, now.Add(27*time.Hour), cfg)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else {
			var pgerr interface{ SQLState() string }
			require.ErrorAs(t, e, &pgerr)
			require.Equal(t, "40001", pgerr.SQLState())
		}
	}
	require.Positive(t, success)
	var body []byte
	require.NoError(t, pool.QueryRow(ctx, `select receipt from catalogue_policy_shadow where info_hash=$1`, hash).Scan(&body))
	var stored Receipt
	require.NoError(t, json.Unmarshal(body, &stored))
	require.Equal(t, "shadow", stored.Mode)
	require.Len(t, stored.Input.Observations, 4)
}

func TestPostgresPolicyShadowPackAndPublicClientEvidence(t *testing.T) {
	pool := policyTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	cfg := DefaultAvailabilityConfig()
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := make([]byte, 20)
	hash[19] = 2
	_, err := pool.Exec(ctx, `insert into torrents values($1,'Synthetic.Pack.2031.1080p.English.Audio',$2,false,2);
insert into torrent_files values($1,0,'A.2031.1080p.English.Audio.mkv',$2),($1,1,'B.2031.1080p.mkv',$2);
insert into label_evidence(info_hash,source,source_kind,category,observed_at,raw_payload) values
($1,'qbittorrent','qb_state_observation','private',$2,'{"state":"seeding"}'),
($1,'qbittorrent','qb_state_observation','movie',$2,'{"state":"error"}')`, hash, now)
	require.NoError(t, err)
	r, err := store.Evaluate(ctx, hash, true, true, now, cfg)
	require.NoError(t, err)
	require.Equal(t, "mixed_review", r.English.State)
	require.Equal(t, "unknown_stale", r.Availability.State)
	require.Len(t, r.Input.Observations, 1, "private client observation excluded")
	_, err = pool.Exec(ctx, `insert into label_evidence(info_hash,source,source_kind,category,observed_at,raw_payload) values($1,'qbittorrent','qb_state_observation','movie',$2,'{"state":"seeding"}')`, hash, now.Add(time.Minute))
	require.NoError(t, err)
	r, err = store.Evaluate(ctx, hash, true, true, now.Add(2*time.Minute), cfg)
	require.NoError(t, err)
	require.Equal(t, "fresh_positive", r.Availability.State)
	_, err = pool.Exec(ctx, `insert into torrents values(decode(repeat('ff',20),'hex'),'Private.2031',now(),true,1)`)
	require.NoError(t, err)
	candidates, err := store.Candidates(ctx, []byte{}, 128)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	_, err = store.Candidates(ctx, []byte{}, 129)
	require.Error(t, err)
}

func TestPostgresPackIncludesSupportedVideoMembersAndHeaderVeto(t *testing.T) {
	pool := policyTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	extensions := append(append([]string(nil), model.FileTypeVideo.Extensions()...), "m2ts")
	for i, ext := range extensions {
		t.Run(ext, func(t *testing.T) {
			hash := make([]byte, 20)
			hash[19] = byte(i + 20)
			_, err := pool.Exec(ctx, `insert into torrents values($1,'Synthetic.Pack.2031.1080p.English.Audio',$2,false,2);
insert into torrent_files values($1,0,$3,$2),($1,1,$4,$2)`, hash, now, "A.2031.1080p.English.Audio."+ext, "B.2031.1080p.Japanese.Audio.Only."+ext)
			require.NoError(t, err)
			r, err := store.Evaluate(ctx, hash, false, false, now, DefaultAvailabilityConfig())
			require.NoError(t, err)
			require.Equal(t, "mixed_review", r.English.State)
			require.Len(t, r.English.Evidence, 3, "retain header and both original member claims")
			_, err = pool.Exec(ctx, `update torrents set name='Synthetic.Pack.2031.1080p.No.English.Audio' where info_hash=$1;
update torrent_files set path=$2 where info_hash=$1 and index=1`, hash, "B.2031.1080p.English.Audio."+ext)
			require.NoError(t, err)
			r, err = store.Evaluate(ctx, hash, false, false, now, DefaultAvailabilityConfig())
			require.NoError(t, err)
			require.Equal(t, "conflicting", r.English.State)
			require.Equal(t, TrackNo, r.English.Evidence[0].Audio)
			require.Equal(t, TrackYes, r.English.Evidence[1].Audio)
			_, err = pool.Exec(ctx, `update torrents set name='Synthetic.Pack.2031.1080p.English.Audio' where info_hash=$1;
update torrent_files set path=$2 where info_hash=$1`, hash, "Unadvertised.2031.1080p."+ext)
			require.NoError(t, err)
			r, err = store.Evaluate(ctx, hash, false, false, now, DefaultAvailabilityConfig())
			require.NoError(t, err)
			require.Equal(t, "unknown", r.English.State, "a positive header cannot qualify missing member evidence")
		})
	}
}

func TestPostgresRecentPublicPositiveSurvivesHistoryCapAndReceiptMerge(t *testing.T) {
	pool := policyTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	cfg := DefaultAvailabilityConfig()
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash := make([]byte, 20)
	hash[19] = 90
	_, err := pool.Exec(ctx, `insert into torrents values($1,'Synthetic.Film.2031.1080p',$2,false,1);
insert into label_evidence(info_hash,source,source_kind,category,observed_at,raw_payload) values
($1,'qbittorrent','qb_state_observation','movie',$3,'{"state":"seeding"}'),
($1,'qbittorrent','qb_state_observation','private',$4,'{"state":"seeding"}'),
($1,'qbittorrent','qb_state_observation','movie',$5,'{"state":"seeding"}');
insert into label_evidence(info_hash,source,source_kind,category,observed_at,raw_payload)
select $1,'qbittorrent','qb_state_observation','movie',$2::timestamptz - (i*interval '5 minutes' + interval '1 minute'),'{"state":"stalledDL","network_healthy":true}'::jsonb from generate_series(0,63) as s(i)`, hash, now, now.Add(-36*time.Hour), now.Add(-time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		r, err := store.Evaluate(ctx, hash, true, false, now.Add(time.Duration(i)*time.Minute), cfg)
		require.NoError(t, err)
		require.Len(t, r.Input.Observations, maxObservations)
		require.Equal(t, "unknown_stale", r.Availability.State)
		require.Equal(t, "recent_positive_history_protects", r.Availability.Reason)
		require.True(t, now.Add(-36*time.Hour).Equal(r.Input.Observations[maxObservations-1].ObservedAt))
		_, err = pool.Exec(ctx, `delete from label_evidence where info_hash=$1 and observed_at=$2`, hash, now.Add(-36*time.Hour))
		require.NoError(t, err)
	}
}
