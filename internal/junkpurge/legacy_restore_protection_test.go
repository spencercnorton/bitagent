package junkpurge

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPostgresLegacyRestoreProtectsCurrentAuthorityAndSource(t *testing.T) {
	for _, kind := range []string{"private", "canonical", "wanted", "manual", "reference", "bitgrab", "qb_private", "qb_bitgrab", "csam_verdict", "recrawled_source", "foreign_snapshot_hash"} {
		t.Run(kind, func(t *testing.T) {
			ctx, pool, vs := expiryIntegrationPool(t)
			h := expiryHash(42)
			seedExpirySnapshot(t, ctx, pool, vs, h)
			want := cataloguerecovery.ErrProtected
			var sql string
			switch kind {
			case "private":
				sql = `UPDATE junkpurge_quarantine SET torrent_snapshot=jsonb_set(torrent_snapshot,'{private}','true') WHERE info_hash=$1`
			case "canonical":
				sql = `INSERT INTO torrent_canonical_labels(info_hash,resolved_source,resolved_strength,resolved_at) VALUES($1,'synthetic-authority',10,now())`
			case "wanted", "manual", "reference", "bitgrab":
				sql = `INSERT INTO torrents(info_hash,name,size,private,created_at,updated_at,files_status,files_count)
SELECT info_hash,name,size,private,created_at,updated_at,files_status,files_count
FROM jsonb_populate_record(null::torrents,(SELECT torrent_snapshot FROM junkpurge_quarantine WHERE info_hash=$1));
INSERT INTO torrent_tags(info_hash,name,created_at,updated_at) VALUES($1,'` + kind + `',now(),now())`
			case "qb_private", "qb_bitgrab":
				category := " PRIVATE "
				if kind == "qb_bitgrab" {
					category = " BITGRAB "
				}
				_, err := pool.Exec(ctx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength) VALUES('qbittorrent','synthetic-privacy','fixture',encode($1::bytea,'hex'),$1,$2,now(),10)`, h, category)
				require.NoError(t, err)
			case "csam_verdict":
				require.NoError(t, vs.Record(ctx, verdicts.Event{InfoHash: h, Verdict: verdicts.VerdictBlacklisted, Mechanism: verdicts.MechanismCsam, Reason: "synthetic competing authority"}))
			case "recrawled_source":
				want = cataloguerecovery.ErrConflict
				sql = `INSERT INTO torrents(info_hash,name,size,private,created_at,updated_at,files_status,files_count)
SELECT info_hash,'ChangedSyntheticSource.mkv',size,private,created_at,updated_at,files_status,files_count
FROM jsonb_populate_record(null::torrents,(SELECT torrent_snapshot FROM junkpurge_quarantine WHERE info_hash=$1))`
			case "foreign_snapshot_hash":
				want = cataloguerecovery.ErrConflict
				sql = `UPDATE junkpurge_quarantine SET torrent_snapshot=jsonb_set(torrent_snapshot,'{info_hash}',to_jsonb(decode(repeat('01',20),'hex'))) WHERE info_hash=$1`
			}
			if sql != "" {
				_, err := pool.Exec(ctx, sql, pgx.QueryExecModeSimpleProtocol, h)
				require.NoError(t, err)
			}
			before := expirySnapshot(t, ctx, pool, h)
			ledgerState, ledgerEvents := expiryLedger(t, ctx, pool, h)
			var rawBefore, rawAfter string
			require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(t)),'[]'::jsonb)::text FROM torrents t`).Scan(&rawBefore))
			err := RestoreQuarantined(ctx, pool, vs, nil, zap.NewNop().Sugar(), hex.EncodeToString(h))
			require.ErrorIs(t, err, want)
			require.Equal(t, before, expirySnapshot(t, ctx, pool, h))
			state, events := expiryLedger(t, ctx, pool, h)
			require.Equal(t, ledgerState, state)
			require.Equal(t, ledgerEvents, events)
			require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(t)),'[]'::jsonb)::text FROM torrents t`).Scan(&rawAfter))
			require.Equal(t, rawBefore, rawAfter)
			var queued int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM queue_jobs`).Scan(&queued))
			require.Zero(t, queued)
		})
	}
}

func TestPostgresLegacyRestoreSeesPrivateAuthorityBeforeFirstSnapshot(t *testing.T) {
	ctx, pool, vs := expiryIntegrationPool(t)
	h := expiryHash(43)
	before := seedExpirySnapshot(t, ctx, pool, vs, h)
	actor, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = actor.Rollback(ctx) })
	_, err = actor.Exec(ctx, `LOCK TABLE label_evidence IN ROW EXCLUSIVE MODE`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		done <- RestoreQuarantined(ctx, pool, vs, nil, zap.NewNop().Sugar(), hex.EncodeToString(h))
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%LOCK TABLE label_evidence,torrent_canonical_labels%')`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond)
	_, err = actor.Exec(ctx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength) VALUES('qbittorrent','synthetic-privacy','fixture',encode($1::bytea,'hex'),$1,'private',now(),10)`, h)
	require.NoError(t, err)
	require.NoError(t, actor.Commit(ctx))
	require.ErrorIs(t, <-done, cataloguerecovery.ErrProtected)
	require.Equal(t, before, expirySnapshot(t, ctx, pool, h))
	var changed bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM torrents) OR EXISTS(SELECT 1 FROM queue_jobs) OR EXISTS(SELECT 1 FROM torrent_verdict_events WHERE verdict='restored')`).Scan(&changed))
	require.False(t, changed)
}

func TestPostgresLegacyRestoreDoesNotProjectOversizedSnapshot(t *testing.T) {
	for _, kind := range []string{"bytes", "rows"} {
		t.Run(kind, func(t *testing.T) {
			ctx, pool, vs := expiryIntegrationPool(t)
			h := expiryHash(44)
			seedExpirySnapshot(t, ctx, pool, vs, h)
			update := `UPDATE junkpurge_quarantine SET files_snapshot=(SELECT jsonb_agg(0) FROM generate_series(1,65536)) WHERE info_hash=$1`
			if kind == "bytes" {
				update = `UPDATE junkpurge_quarantine SET torrent_snapshot=jsonb_set(torrent_snapshot,'{name}',to_jsonb(repeat('x',67108865))) WHERE info_hash=$1`
			}
			_, err := pool.Exec(ctx, update, h)
			require.NoError(t, err)
			var torrent, files, sources []byte
			var observed time.Time
			var snapshotBytes, snapshotRows int64
			require.NoError(t, pool.QueryRow(ctx, boundedLegacySnapshotSQL, h, legacySnapshotMaxBytes, legacySnapshotMaxRows).Scan(&torrent, &files, &sources, &observed, &snapshotBytes, &snapshotRows))
			require.Nil(t, torrent)
			require.Nil(t, files)
			require.Nil(t, sources)
			err = RestoreQuarantined(ctx, pool, vs, nil, zap.NewNop().Sugar(), hex.EncodeToString(h))
			require.ErrorIs(t, err, cataloguerecovery.ErrCapacity)
			var modified bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM torrents) OR EXISTS(SELECT 1 FROM queue_jobs) OR NOT EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=$1)`, h).Scan(&modified))
			require.False(t, modified)
		})
	}
}

func TestPostgresLegacyRestoreBoundsSnapshotRefreshedBeforeLock(t *testing.T) {
	ctx, pool, vs := expiryIntegrationPool(t)
	h := expiryHash(45)
	seedExpirySnapshot(t, ctx, pool, vs, h)
	_, err := pool.Exec(ctx, `CREATE FUNCTION synthetic_restore_capacity_barrier() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_advisory_xact_lock(9459,1); RETURN NEW; END$$;
CREATE TRIGGER restore_capacity_barrier BEFORE INSERT ON torrents FOR EACH ROW EXECUTE FUNCTION synthetic_restore_capacity_barrier()`)
	require.NoError(t, err)
	actor, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = actor.Rollback(ctx) })
	_, err = actor.Exec(ctx, `SELECT pg_advisory_xact_lock(9459,1)`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- RestoreQuarantined(ctx, pool, vs, nil, zap.NewNop().Sugar(), hex.EncodeToString(h)) }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%INSERT INTO torrents (info_hash, name, size, private%')`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond)
	_, err = actor.Exec(ctx, `UPDATE junkpurge_quarantine SET files_snapshot=(SELECT jsonb_agg(0) FROM generate_series(1,65536)) WHERE info_hash=$1`, h)
	require.NoError(t, err)
	require.NoError(t, actor.Commit(ctx))
	require.ErrorIs(t, <-done, cataloguerecovery.ErrCapacity)
	var modified bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM torrents) OR EXISTS(SELECT 1 FROM queue_jobs) OR EXISTS(SELECT 1 FROM torrent_verdict_events WHERE verdict='restored')`).Scan(&modified))
	require.False(t, modified)
}
