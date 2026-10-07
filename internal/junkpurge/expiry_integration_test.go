package junkpurge

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func expiryIntegrationPool(t *testing.T) (context.Context, *pgxpool.Pool, *verdicts.Store) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to run PostgreSQL integration coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	schema := fmt.Sprintf("quarantine_expiry_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
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
		cleanup, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = cleanup.Close(context.Background())
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	vs := verdicts.NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	return ctx, pool, vs
}

func expiryHash(n uint64) []byte {
	h := make([]byte, 20)
	binary.BigEndian.PutUint64(h[12:], n)
	return h
}

func seedExpirySnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, vs *verdicts.Store, hash []byte) string {
	t.Helper()
	_, err := pool.Exec(ctx, `
INSERT INTO torrents (info_hash,name,size,private,files_status,files_count,created_at,updated_at)
VALUES ($1,'SyntheticFixture.ENG.mkv',4096,false,'multi',1,now()-interval '60 days',now()-interval '40 days');
INSERT INTO torrent_files (info_hash,"index",path,size,created_at,updated_at)
VALUES ($1,0,'SyntheticFixture/SyntheticFixture.ENG.mkv',4096,now(),now());
INSERT INTO torrent_sources (key,name,created_at,updated_at)
VALUES ('fixture','Synthetic fixture',now(),now()) ON CONFLICT DO NOTHING;
INSERT INTO torrents_torrent_sources (source,info_hash,seeders,published_at,created_at,updated_at)
VALUES ('fixture',$1,2,now()-interval '40 days',now(),now());
INSERT INTO junkpurge_judgments (info_hash,torrent_name,verdict,confidence)
VALUES ($1,'SyntheticFixture.ENG.mkv','junk',0.99)`, pgx.QueryExecModeSimpleProtocol, hash)
	require.NoError(t, err)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	hashes, err := quarantineJunkTx(ctx, tx, [][]byte{hash}, 0.8)
	require.NoError(t, err)
	require.Len(t, hashes, 1)
	require.NoError(t, tx.Commit(ctx))
	_, err = pool.Exec(ctx, `UPDATE junkpurge_quarantine SET quarantined_at=now()-interval '40 days' WHERE info_hash=$1`, hash)
	require.NoError(t, err)
	require.NoError(t, vs.Record(ctx, verdicts.Event{InfoHash: hash, Verdict: verdicts.VerdictQuarantined, Mechanism: verdicts.MechanismJunkpurge}))
	return expirySnapshot(t, ctx, pool, hash)
}

func expirySnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hash []byte) string {
	t.Helper()
	var snapshot string
	require.NoError(t, pool.QueryRow(ctx, `SELECT jsonb_build_array(torrent_snapshot,files_snapshot,sources_snapshot)::text FROM junkpurge_quarantine WHERE info_hash=$1`, hash).Scan(&snapshot))
	return snapshot
}

func expiryLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hash []byte) (string, []string) {
	t.Helper()
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT verdict FROM torrent_verdict_state WHERE info_hash=$1`, hash).Scan(&state))
	rows, err := pool.Query(ctx, `SELECT verdict FROM torrent_verdict_events WHERE info_hash=$1 ORDER BY id`, hash)
	require.NoError(t, err)
	defer rows.Close()
	var events []string
	for rows.Next() {
		var verdict string
		require.NoError(t, rows.Scan(&verdict))
		events = append(events, verdict)
	}
	require.NoError(t, rows.Err())
	return state, events
}

func TestQuarantineExpiryAtomicIntegration(t *testing.T) {
	t.Run("restore completes before expiry", func(t *testing.T) {
		ctx, pool, vs := expiryIntegrationPool(t)
		h := expiryHash(1)
		seedExpirySnapshot(t, ctx, pool, vs, h)
		require.NoError(t, RestoreQuarantined(ctx, pool, vs, zap.NewNop().Sugar(), hex.EncodeToString(h)))
		n, err := expireQuarantineChunk(ctx, pool, 30, 1, true)
		require.NoError(t, err)
		require.Zero(t, n)
		state, events := expiryLedger(t, ctx, pool, h)
		require.Equal(t, verdicts.VerdictRestored, state)
		require.Equal(t, []string{verdicts.VerdictQuarantined, verdicts.VerdictRestored}, events)
		var recovered bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM torrents WHERE info_hash=$1)
AND EXISTS(SELECT 1 FROM torrent_files WHERE info_hash=$1)
AND EXISTS(SELECT 1 FROM torrents_torrent_sources WHERE info_hash=$1 AND source='fixture' AND seeders=2)
AND EXISTS(SELECT 1 FROM queue_jobs WHERE queue='process_torrent')`, h).Scan(&recovered))
		require.True(t, recovered)
	})

	t.Run("expiry lock serializes restore and event order", func(t *testing.T) {
		ctx, pool, vs := expiryIntegrationPool(t)
		h := expiryHash(2)
		snapshot := seedExpirySnapshot(t, ctx, pool, vs, h)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		n, err := expireQuarantineChunkTx(ctx, tx, 30, 1, true)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		// An uncommitted expiry owns the actual snapshot row. A concurrent
		// operator cannot remove it and append a restore before that commit.
		result := make(chan error, 1)
		go func() { result <- RestoreQuarantined(ctx, pool, vs, zap.NewNop().Sugar(), hex.EncodeToString(h)) }()
		var pid int
		require.NoError(t, tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
		require.Eventually(t, func() bool {
			var waiting bool
			err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting)
			return err == nil && waiting
		}, 5*time.Second, 10*time.Millisecond)
		require.Equal(t, snapshot, expirySnapshot(t, ctx, pool, h))
		require.NoError(t, tx.Commit(ctx))
		require.NoError(t, <-result)
		state, events := expiryLedger(t, ctx, pool, h)
		require.Equal(t, verdicts.VerdictRestored, state)
		require.Equal(t, []string{verdicts.VerdictQuarantined, verdicts.VerdictTombstoned, verdicts.VerdictRestored}, events)
	})

	t.Run("locked and refreshed snapshots are not expired", func(t *testing.T) {
		ctx, pool, vs := expiryIntegrationPool(t)
		h := expiryHash(3)
		seedExpirySnapshot(t, ctx, pool, vs, h)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, `INSERT INTO torrents (info_hash,name,size,private,created_at,updated_at,files_status)
VALUES ($1,'SyntheticRefresh',8192,false,now(),now(),'single');
UPDATE junkpurge_judgments SET torrent_name='SyntheticRefresh',purged=false WHERE info_hash=$1`, pgx.QueryExecModeSimpleProtocol, h)
		require.NoError(t, err)
		refreshed, err := quarantineJunkTx(ctx, tx, [][]byte{h}, 0.8)
		require.NoError(t, err)
		require.Len(t, refreshed, 1, "exercise actual recrawl/re-quarantine refresh")
		n, err := expireQuarantineChunk(ctx, pool, 30, 1, true)
		require.NoError(t, err)
		require.Zero(t, n, "skip the refresh lock rather than retain a stale expiry list")
		require.NoError(t, tx.Commit(ctx))
		n, err = expireQuarantineChunk(ctx, pool, 30, 1, true)
		require.NoError(t, err)
		require.Zero(t, n, "recheck the refreshed review window")
		state, events := expiryLedger(t, ctx, pool, h)
		require.Equal(t, verdicts.VerdictQuarantined, state)
		require.Equal(t, []string{verdicts.VerdictQuarantined}, events)
	})

	t.Run("restore and recrawl quarantine take the same raw lock first", func(t *testing.T) {
		ctx, pool, vs := expiryIntegrationPool(t)
		h := expiryHash(9)
		seedExpirySnapshot(t, ctx, pool, vs, h)
		_, err := pool.Exec(ctx, `INSERT INTO torrents (info_hash,name,size,private,created_at,updated_at,files_status)
VALUES ($1,'SyntheticReacquired',8192,false,now(),now(),'single');
UPDATE junkpurge_judgments SET torrent_name='SyntheticReacquired',purged=false WHERE info_hash=$1`, pgx.QueryExecModeSimpleProtocol, h)
		require.NoError(t, err)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		var pid int
		require.NoError(t, tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM torrents WHERE info_hash=$1 FOR UPDATE`, h).Scan(&pid))
		result := make(chan error, 1)
		go func() { result <- RestoreQuarantined(ctx, pool, vs, zap.NewNop().Sugar(), hex.EncodeToString(h)) }()
		require.Eventually(t, func() bool {
			var waiting bool
			err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting)
			return err == nil && waiting
		}, 5*time.Second, 10*time.Millisecond)
		// Restore is waiting on the raw row, without holding the snapshot that
		// this actual quarantine transaction needs next. Neither side deadlocks.
		quarantined, err := quarantineJunkTx(ctx, tx, [][]byte{h}, 0.8)
		require.NoError(t, err)
		require.Len(t, quarantined, 1)
		require.NoError(t, tx.Commit(ctx))
		require.ErrorContains(t, <-result, "lock restored torrent")
		var name string
		require.NoError(t, pool.QueryRow(ctx, `SELECT torrent_snapshot->>'name' FROM junkpurge_quarantine WHERE info_hash=$1`, h).Scan(&name))
		require.Equal(t, "SyntheticReacquired", name)
		require.NoError(t, RestoreQuarantined(ctx, pool, vs, zap.NewNop().Sugar(), hex.EncodeToString(h)))
		require.NoError(t, pool.QueryRow(ctx, `SELECT name FROM torrents WHERE info_hash=$1`, h).Scan(&name))
		require.Equal(t, "SyntheticReacquired", name, "a subsequent explicit restore uses the refreshed snapshot")
	})

	t.Run("restore rejects a changed snapshot rather than consume its old proposal", func(t *testing.T) {
		ctx, pool, vs := expiryIntegrationPool(t)
		h := expiryHash(10)
		seedExpirySnapshot(t, ctx, pool, vs, h)
		_, err := pool.Exec(ctx, `INSERT INTO torrents (info_hash,name,size,private,created_at,updated_at,files_status)
VALUES ($1,'SyntheticCurrent',8192,false,now(),now(),'single')`, h)
		require.NoError(t, err)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		var pid int
		require.NoError(t, tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM torrents WHERE info_hash=$1 FOR UPDATE`, h).Scan(&pid))
		result := make(chan error, 1)
		go func() { result <- RestoreQuarantined(ctx, pool, vs, zap.NewNop().Sugar(), hex.EncodeToString(h)) }()
		require.Eventually(t, func() bool {
			var waiting bool
			err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting)
			return err == nil && waiting
		}, 5*time.Second, 10*time.Millisecond)
		_, err = tx.Exec(ctx, `UPDATE junkpurge_quarantine SET quarantined_at=now(),
torrent_snapshot=jsonb_set(torrent_snapshot,'{name}','"SyntheticRefreshed"'::jsonb) WHERE info_hash=$1`, h)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		require.ErrorContains(t, <-result, "snapshot changed during restore")
		var retained bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=$1)
AND NOT EXISTS(SELECT 1 FROM queue_jobs)
AND NOT EXISTS(SELECT 1 FROM torrent_verdict_events WHERE info_hash=$1 AND verdict='restored')`, h).Scan(&retained))
		require.True(t, retained)
	})

	for _, transition := range []string{"restore", "delete", "expire"} {
		t.Run("delayed admission observation preserves "+transition, func(t *testing.T) {
			ctx, pool, vs := expiryIntegrationPool(t)
			h := expiryHash(7)
			seedExpirySnapshot(t, ctx, pool, vs, h)
			expected := verdicts.VerdictTombstoned
			switch transition {
			case "restore":
				require.NoError(t, RestoreQuarantined(ctx, pool, vs, zap.NewNop().Sugar(), hex.EncodeToString(h)))
				expected = verdicts.VerdictRestored
			case "delete":
				// Construct a historical operator deletion to verify delayed
				// observations cannot overwrite it. The live writer is held.
				tx, err := pool.Begin(ctx)
				require.NoError(t, err)
				_, err = tx.Exec(ctx, `DELETE FROM junkpurge_quarantine WHERE info_hash=$1`, h)
				require.NoError(t, err)
				require.NoError(t, verdicts.RecordTx(ctx, tx, verdicts.Event{InfoHash: h, Verdict: verdicts.VerdictBlacklisted, Mechanism: verdicts.MechanismOperator, Reason: "synthetic historical deletion"}))
				require.NoError(t, tx.Commit(ctx))
				expected = verdicts.VerdictBlacklisted
			case "expire":
				n, err := expireQuarantineChunk(ctx, pool, 30, 1, true)
				require.NoError(t, err)
				require.Equal(t, 1, n)
			}
			require.NoError(t, recordQuarantineVerdict(ctx, pool, h, 30, "synthetic delayed admission"))
			state, events := expiryLedger(t, ctx, pool, h)
			require.Equal(t, expected, state)
			require.Equal(t, []string{verdicts.VerdictQuarantined, expected}, events)
		})
	}

	t.Run("admission observation uses snapshot deadline and deduplicates", func(t *testing.T) {
		ctx, pool, vs := expiryIntegrationPool(t)
		h := expiryHash(8)
		seedExpirySnapshot(t, ctx, pool, vs, h)
		_, err := pool.Exec(ctx, `UPDATE junkpurge_quarantine SET quarantined_at=now() WHERE info_hash=$1`, h)
		require.NoError(t, err)
		require.NoError(t, recordQuarantineVerdict(ctx, pool, h, 30, "synthetic admission"))
		require.NoError(t, recordQuarantineVerdict(ctx, pool, h, 30, "synthetic repeated admission"))
		_, events := expiryLedger(t, ctx, pool, h)
		require.Len(t, events, 2, "one current snapshot observation, not one per caller")
		var sameDeadline bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT s.expires_at=q.quarantined_at+interval '30 days'
FROM torrent_verdict_state s JOIN junkpurge_quarantine q USING(info_hash) WHERE info_hash=$1`, h).Scan(&sameDeadline))
		require.True(t, sameDeadline)
	})

	for _, failure := range []string{"state", "event", "commit"} {
		t.Run(failure+" failure rolls back marker and retries once", func(t *testing.T) {
			ctx, pool, vs := expiryIntegrationPool(t)
			h := expiryHash(4)
			snapshot := seedExpirySnapshot(t, ctx, pool, vs, h)
			_, err := pool.Exec(ctx, `CREATE FUNCTION synthetic_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic ledger failure'; END $$`)
			require.NoError(t, err)
			table := "torrent_verdict_events"
			trigger := "CREATE TRIGGER reject_transition BEFORE INSERT ON torrent_verdict_events FOR EACH ROW EXECUTE FUNCTION synthetic_failure()"
			if failure == "state" {
				table = "torrent_verdict_state"
				trigger = "CREATE TRIGGER reject_transition BEFORE INSERT OR UPDATE ON torrent_verdict_state FOR EACH ROW EXECUTE FUNCTION synthetic_failure()"
			} else if failure == "commit" {
				trigger = "CREATE CONSTRAINT TRIGGER reject_transition AFTER INSERT ON torrent_verdict_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION synthetic_failure()"
			}
			_, err = pool.Exec(ctx, trigger)
			require.NoError(t, err)
			n, err := expireQuarantineChunk(ctx, pool, 30, 1, true)
			require.Error(t, err)
			require.Zero(t, n)
			var due bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT expired_at IS NULL FROM junkpurge_quarantine WHERE info_hash=$1`, h).Scan(&due))
			require.True(t, due)
			require.Equal(t, snapshot, expirySnapshot(t, ctx, pool, h))
			state, events := expiryLedger(t, ctx, pool, h)
			require.Equal(t, verdicts.VerdictQuarantined, state)
			require.Len(t, events, 1)
			_, err = pool.Exec(ctx, "DROP TRIGGER reject_transition ON "+table)
			require.NoError(t, err)
			n, err = expireQuarantineChunk(ctx, pool, 30, 1, true)
			require.NoError(t, err)
			require.Equal(t, 1, n)
			n, err = expireQuarantineChunk(ctx, pool, 30, 1, true)
			require.NoError(t, err)
			require.Zero(t, n)
			state, events = expiryLedger(t, ctx, pool, h)
			require.Equal(t, verdicts.VerdictTombstoned, state)
			require.Equal(t, []string{verdicts.VerdictQuarantined, verdicts.VerdictTombstoned}, events)
		})
	}

	t.Run("cancel inside ledger write retains durable retry", func(t *testing.T) {
		ctx, pool, vs := expiryIntegrationPool(t)
		h := expiryHash(5)
		seedExpirySnapshot(t, ctx, pool, vs, h)
		_, err := pool.Exec(ctx, `CREATE FUNCTION synthetic_barrier() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(701234); RETURN NEW; END $$;
CREATE TRIGGER expiry_barrier BEFORE INSERT ON torrent_verdict_events FOR EACH ROW EXECUTE FUNCTION synthetic_barrier()`)
		require.NoError(t, err)
		keeper, err := pool.Acquire(ctx)
		require.NoError(t, err)
		defer keeper.Release()
		_, err = keeper.Exec(ctx, `SELECT pg_advisory_lock(701234)`)
		require.NoError(t, err)
		defer func() { _, _ = keeper.Exec(context.Background(), `SELECT pg_advisory_unlock(701234)`) }()
		attempt, cancel := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() { _, err := expireQuarantineChunk(attempt, pool, 30, 1, true); result <- err }()
		require.Eventually(t, func() bool {
			var waiting bool
			err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND objid=701234 AND NOT granted)`).Scan(&waiting)
			return err == nil && waiting
		}, 5*time.Second, 10*time.Millisecond)
		cancel()
		require.Error(t, <-result)
		_, err = keeper.Exec(ctx, `SELECT pg_advisory_unlock(701234)`)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `DROP TRIGGER expiry_barrier ON torrent_verdict_events`)
		require.NoError(t, err)
		var due bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT expired_at IS NULL FROM junkpurge_quarantine WHERE info_hash=$1`, h).Scan(&due))
		require.True(t, due)
		n, err := expireQuarantineChunk(ctx, pool, 30, 1, true)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		_, events := expiryLedger(t, ctx, pool, h)
		require.Equal(t, []string{verdicts.VerdictQuarantined, verdicts.VerdictTombstoned}, events)
	})

	for _, operation := range []string{"restore", "delete"} {
		t.Run(operation+" audit failure rolls back operator transition", func(t *testing.T) {
			ctx, pool, vs := expiryIntegrationPool(t)
			h := expiryHash(6)
			snapshot := seedExpirySnapshot(t, ctx, pool, vs, h)
			_, err := pool.Exec(ctx, `CREATE FUNCTION synthetic_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic ledger failure'; END $$;
CREATE TRIGGER reject_transition BEFORE INSERT ON torrent_verdict_events FOR EACH ROW EXECUTE FUNCTION synthetic_failure()`)
			require.NoError(t, err)
			if operation == "restore" {
				err = RestoreQuarantined(ctx, pool, vs, zap.NewNop().Sugar(), hex.EncodeToString(h))
				require.ErrorContains(t, err, "record quarantine restore")
			} else {
				err = DeleteQuarantinedNow(ctx, pool, vs, zap.NewNop().Sugar(), hex.EncodeToString(h))
				require.ErrorIs(t, err, cataloguerecovery.ErrDisabled)
			}
			require.Error(t, err)
			require.Equal(t, snapshot, expirySnapshot(t, ctx, pool, h))
			var rollback bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM torrents WHERE info_hash=$1)
AND NOT EXISTS(SELECT 1 FROM torrent_liveness WHERE info_hash=$1)
AND NOT EXISTS(SELECT 1 FROM queue_jobs)`, h).Scan(&rollback))
			require.True(t, rollback)
			state, events := expiryLedger(t, ctx, pool, h)
			require.Equal(t, verdicts.VerdictQuarantined, state)
			require.Len(t, events, 1)
		})
	}

	t.Run("bounded backlog converges across two workers", func(t *testing.T) {
		ctx, pool, vs := expiryIntegrationPool(t)
		const backlog = 5000
		_, err := pool.Exec(ctx, `INSERT INTO junkpurge_quarantine
(info_hash,torrent_name,verdict,confidence,quarantined_at,torrent_snapshot,files_snapshot,sources_snapshot)
SELECT decode(lpad(to_hex(n),40,'0'),'hex'),'SyntheticBacklog','junk',0.99,now()-interval '40 days','{}'::jsonb,'[]'::jsonb,'[]'::jsonb FROM generate_series(1,$1) n`, backlog)
		require.NoError(t, err)
		newWorker := func() *purgeWorker {
			return &purgeWorker{cfg: Config{QuarantineDays: 30}, verdicts: vs, metrics: NewMetrics(), logger: zap.NewNop().Sugar()}
		}
		w := newWorker()
		start := time.Now()
		w.expireQuarantine(ctx, pool)
		var marked int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM junkpurge_quarantine WHERE expired_at IS NOT NULL`).Scan(&marked))
		require.Equal(t, quarantineExpiryCycleLimit, marked, "the archive must not drain ahead of candidate work")
		t.Logf("bounded cycle committed %d of %d snapshots in %s", marked, backlog, time.Since(start))
		for round := 0; marked < backlog && round < 10; round++ {
			var workers sync.WaitGroup
			for n := 0; n < 2; n++ {
				workers.Add(1)
				go func() { defer workers.Done(); newWorker().expireQuarantine(ctx, pool) }()
			}
			workers.Wait()
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM junkpurge_quarantine WHERE expired_at IS NOT NULL`).Scan(&marked))
		}
		require.Equal(t, backlog, marked)
		var coherent bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM junkpurge_quarantine)=$1
AND (SELECT count(*) FROM torrent_verdict_events WHERE verdict='tombstoned')=$1
AND (SELECT count(DISTINCT info_hash) FROM torrent_verdict_events WHERE verdict='tombstoned')=$1
AND (SELECT count(*) FROM torrent_verdict_state WHERE verdict='tombstoned')=$1
AND NOT EXISTS(SELECT 1 FROM torrent_liveness)`, backlog).Scan(&coherent))
		require.True(t, coherent, "all retained markers, events and states agree exactly once")
	})

	t.Run("maintenance uses the live index without scanning the retained archive", func(t *testing.T) {
		ctx, pool, _ := expiryIntegrationPool(t)
		_, err := pool.Exec(ctx, `INSERT INTO junkpurge_quarantine
(info_hash,torrent_name,verdict,confidence,quarantined_at,expired_at,torrent_snapshot)
SELECT decode(md5(n::text)||'00000000','hex'),'SyntheticArchive','junk',0.99,
now()-interval '40 days',CASE WHEN n<=100000 THEN now() ELSE NULL END,'{}'::jsonb
FROM generate_series(1,100064) n;
ANALYZE junkpurge_quarantine`)
		require.NoError(t, err)
		var raw []byte
		require.NoError(t, pool.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+quarantineExpiryCandidatesSQL, 30, quarantineExpiryChunkSize).Scan(&raw))
		var plan []map[string]any
		require.NoError(t, json.Unmarshal(raw, &plan))
		require.Contains(t, string(raw), "junkpurge_quarantine_live_idx", "reuse the existing unexpired index rather than scan the tombstone archive")
		node := plan[0]["Plan"].(map[string]any)
		t.Logf("indexed maintenance: rows=%v shared_hits=%v reads=%v execution_ms=%v",
			node["Actual Rows"], node["Shared Hit Blocks"], node["Shared Read Blocks"], plan[0]["Execution Time"])
	})
}
