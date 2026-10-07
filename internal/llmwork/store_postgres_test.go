package llmwork

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/stretchr/testify/require"
)

func workFixture(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("llmwork_test_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	config, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		admin.Close()
	})
	_, err = pool.Exec(ctx, `CREATE TABLE torrents(info_hash bytea PRIMARY KEY,private boolean NOT NULL);
 CREATE TABLE label_evidence(info_hash bytea,source text,category text);
 CREATE TABLE llm_request_budgets(scope text,month_start date,day_start date,daily_calls int,monthly_calls int,PRIMARY KEY(scope,month_start));`)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", "00056_llm_work_lifecycle.sql"))
	require.NoError(t, err)
	up := strings.Split(string(body), "-- +goose Down")[0]
	_, err = pool.Exec(ctx, up)
	require.NoError(t, err)
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.SpreadAdmission = false
	cfg.MaxPending = 24
	pg := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	_, err = pg.Get()
	require.NoError(t, err)
	store, err := NewStore(cfg, pg)
	require.NoError(t, err)
	return store, pool
}

func putPublic(t *testing.T, pool *pgxpool.Pool, d Draft) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO torrents(info_hash,private)VALUES($1,false)ON CONFLICT DO NOTHING`, d.InfoHash)
	require.NoError(t, err)
}

func TestQueueDedupBoundsAndCrossReplicaLease(t *testing.T) {
	s, pool := workFixture(t)
	ctx := context.Background()
	d := draftFor(1)
	putPublic(t, pool, d)
	out, err := s.Enqueue(ctx, d)
	require.NoError(t, err)
	require.Equal(t, "queued", out)
	out, err = s.Enqueue(ctx, d)
	require.NoError(t, err)
	require.Equal(t, "duplicate", out)
	other := draftFor(2)
	putPublic(t, pool, other)
	out, err = s.Enqueue(ctx, other)
	require.NoError(t, err)
	require.Equal(t, "family_sampled", out)
	other.FamilyDigest = Digest("another")
	out, err = s.Enqueue(ctx, other)
	require.NoError(t, err)
	require.Equal(t, "full", out, "hour bucket reserves capacity for later traffic")
	var wg sync.WaitGroup
	leases := make(chan *Lease, 2)
	errs := make(chan error, 2)
	for _, owner := range []string{"first", "second"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			lease, err := s.Claim(ctx, owner)
			if err != nil {
				errs <- err
			}
			if lease != nil {
				leases <- lease
			}
		}(owner)
	}
	wg.Wait()
	close(leases)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var claimed []*Lease
	for lease := range leases {
		claimed = append(claimed, lease)
	}
	require.Len(t, claimed, 1)
	wrong := *claimed[0]
	wrong.Generation++
	require.ErrorIs(t, s.Finish(ctx, wrong, "completed", "test", time.Now()), ErrLease)
	require.NoError(t, s.Finish(ctx, *claimed[0], "deferred", "allowance", time.Now().Add(-time.Second)))
	again, err := s.Claim(ctx, "restart")
	require.NoError(t, err)
	require.NotNil(t, again)
	require.Greater(t, again.Generation, claimed[0].Generation)
	require.NoError(t, s.Finish(ctx, *again, "completed", "test", time.Now()))
}

func TestQueuePrivacyAndUnknownLeaseRecovery(t *testing.T) {
	s, pool := workFixture(t)
	ctx := context.Background()
	d := draftFor(1)
	putPublic(t, pool, d)
	_, err := pool.Exec(ctx, `INSERT INTO label_evidence VALUES($1,'qbittorrent','private')`, d.InfoHash)
	require.NoError(t, err)
	out, err := s.Enqueue(ctx, d)
	require.ErrorIs(t, err, ErrObsolete)
	require.Equal(t, "privacy", out)
	_, err = pool.Exec(ctx, `DELETE FROM label_evidence`)
	require.NoError(t, err)
	_, err = s.Enqueue(ctx, d)
	require.NoError(t, err)
	lease, err := s.Claim(ctx, "crashed")
	require.NoError(t, err)
	require.NotNil(t, lease)
	_, err = pool.Exec(ctx, `UPDATE llm_work_tasks SET lease_until=now()-interval '1 second' WHERE task_key=$1`, lease.Task.Key)
	require.NoError(t, err)
	recovered, err := s.Claim(ctx, "another")
	require.NoError(t, err)
	require.Nil(t, recovered, "unreconciled lease never causes automatic HTTP retry")
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM llm_work_tasks WHERE task_key=$1`, lease.Task.Key).Scan(&state))
	require.Equal(t, "held", state)
}

func TestQueueDisabledOrColdPoolNeverInitializesOnIngest(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	p := lazy.New(func() (*pgxpool.Pool, error) { t.Fatal("ingestion initialized queue pool"); return nil, nil })
	s, err := NewStore(cfg, p)
	require.NoError(t, err)
	_, err = s.Enqueue(context.Background(), draftFor(1))
	require.Error(t, err)
	cfg.Enabled = false
	s, err = NewStore(cfg, p)
	require.NoError(t, err)
	out, err := s.Enqueue(context.Background(), draftFor(1))
	require.NoError(t, err)
	require.Equal(t, "disabled", out)
}

func TestPostgresQualifiedAuthorityKeepsWantedMatchingEligible(t *testing.T) {
	_, pool := workFixture(t)
	ctx := context.Background()
	d := draftFor(7)
	putPublic(t, pool, d)
	_, err := pool.Exec(ctx, `create table torrent_tags(info_hash bytea,name text);create table torrent_canonical_labels(info_hash bytea,media_type text,media_id text)`)
	require.NoError(t, err)
	for _, prefix := range []string{"wanted", "manual", "reference", "bitgrab"} {
		for _, separator := range []string{":", "/", "-", "_"} {
			name := " \t" + strings.ToUpper(prefix) + separator + "synthetic\r\n"
			t.Run(prefix+separator, func(t *testing.T) {
				_, err := pool.Exec(ctx, `insert into torrent_tags values($1,$2)`, d.InfoHash, name)
				require.NoError(t, err)
				tx, err := pool.Begin(ctx)
				require.NoError(t, err)
				err = lockedAuthority(ctx, tx, Task{Draft: d})
				if prefix == "wanted" {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, ErrObsolete)
				}
				require.NoError(t, tx.Rollback(ctx))
				var count int
				require.NoError(t, pool.QueryRow(ctx, `select count(*) from torrent_tags`).Scan(&count))
				require.Equal(t, 1, count)
				_, err = pool.Exec(ctx, `delete from torrent_tags`)
				require.NoError(t, err)
			})
		}
	}
}
