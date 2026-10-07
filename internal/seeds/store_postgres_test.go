package seeds

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/cataloguepolicy"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/stretchr/testify/require"
)

func seedsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN for disposable PostgreSQL coverage")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("seeds_test_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `create schema `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
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
	_, err = pool.Exec(ctx, `create table torrents(info_hash bytea primary key,name text not null default 'Synthetic.Allowed.Release',private boolean not null default false);
create table torrent_tracker_seeds(info_hash bytea primary key,tracker_known boolean,seeders int,leechers int,completed int,best_tracker text,checked_at timestamptz,peak_seeders int,prev_seeders int,last_positive_at timestamptz);
create table torrents_torrent_sources(source text,info_hash bytea,seeders int,leechers int,created_at timestamptz,updated_at timestamptz,primary key(source,info_hash));
create table torrent_liveness(info_hash bytea primary key,status text,last_qb_state text,suspect_first_seen_at timestamptz,suspect_observations int default 0,last_observed_at timestamptz,blacklisted_at timestamptz,next_revalidate_at timestamptz,alive_source text,updated_at timestamptz);
create table label_evidence(info_hash bytea,source text,category text);
create table torrent_canonical_labels(info_hash bytea,category text);
create table torrent_tags(info_hash bytea,name text);
create table torrent_contents(id text primary key,info_hash bytea,content_type text,seeders int,leechers int);`)
	require.NoError(t, err)
	return pool
}

func TestPostgresLeechersPositiveHistoryAndCoverage(t *testing.T) {
	ctx := context.Background()
	pool := seedsTestPool(t)
	store := NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	hash := []byte("synthetic-swarm-0001")
	_, err := pool.Exec(ctx, `insert into torrents(info_hash) values($1)`, hash)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `insert into torrent_liveness(info_hash,status) values($1,'dead')`, hash)
	require.NoError(t, err)
	persist := func(outcome *ScrapeOutcome) {
		t.Helper()
		_, _, err = store.Persist(ctx, [][]byte{hash}, map[string]*ScrapeOutcome{string(hash): outcome})
		require.NoError(t, err)
	}
	readPositive := func() (checked time.Time, positive *time.Time) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `select checked_at,last_positive_at from torrent_tracker_seeds where info_hash=$1`, hash).Scan(&checked, &positive))
		return
	}
	// Both insert and conflict-update paths must retain a leechers-only
	// positive, using the same definition as ScrapeOutcome.Positive().
	persist(&ScrapeOutcome{TrackerKnown: true, Leechers: 1})
	checked, positive := readPositive()
	require.NotNil(t, positive)
	require.Equal(t, checked, *positive)
	old := checked.Add(-48 * time.Hour)
	_, err = pool.Exec(ctx, `update torrent_tracker_seeds set last_positive_at=$2 where info_hash=$1`, hash, old)
	require.NoError(t, err)
	persist(&ScrapeOutcome{TrackerKnown: true, Leechers: 2})
	checked, positive = readPositive()
	require.NotNil(t, positive)
	require.Equal(t, checked, *positive)
	require.True(t, positive.After(old))
	ledger, known, live, err := store.CoverageCounts(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), ledger)
	require.Equal(t, int64(1), known)
	require.Equal(t, int64(1), live)
	anchor := *positive
	for _, outcome := range []*ScrapeOutcome{
		{TrackerKnown: true, Completed: 3}, // known zero
		{},                                 // unknown / failed coverage
	} {
		persist(outcome)
		_, positive = readPositive()
		require.NotNil(t, positive)
		require.Equal(t, anchor, *positive)
		_, _, live, err = store.CoverageCounts(ctx)
		require.NoError(t, err)
		require.Zero(t, live)
	}
	// The retained fact protects against two subsequent spaced zeros in the
	// existing shadow evaluator; persistence itself does not change liveness.
	cfg := cataloguepolicy.DefaultAvailabilityConfig()
	now := anchor.Add(48 * time.Hour)
	obs := func(id, class string, at time.Time) cataloguepolicy.Observation {
		return cataloguepolicy.Observation{ID: id, Source: "tracker", Class: class, ObservedAt: at, ExpiresAt: at.Add(cfg.FreshFor), Qualified: true}
	}
	decision := cataloguepolicy.EvaluateAvailability([]cataloguepolicy.Observation{
		obs("positive", "positive", anchor),
		obs("zero-1", "zero", now.Add(-6*time.Hour)),
		obs("zero-2", "zero", now.Add(-time.Hour)),
	}, now, cfg)
	require.Equal(t, "fresh_known_zero", decision.State)
	require.Equal(t, "recent_positive_history_protects", decision.Reason)
	var status string
	require.NoError(t, pool.QueryRow(ctx, `select status from torrent_liveness where info_hash=$1`, hash).Scan(&status))
	require.Equal(t, "dead", status)
}
