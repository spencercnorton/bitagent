package junkpurge

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"github.com/spencercnorton/bitagent/internal/verdicts"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Compare the complete rows affected by restore, including its snapshot and
// independent authority. A refused restore must roll back the provisional raw
// insert as well as retain all dependent rows and permanent history.
func restorePrivacyState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT jsonb_build_object(
'raw',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY info_hash),'[]') FROM torrents r),
'files',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY info_hash,"index"),'[]') FROM torrent_files r),
'sources',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY source,info_hash),'[]') FROM torrents_torrent_sources r),
'registry',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY key),'[]') FROM torrent_sources r),
'snapshots',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY info_hash),'[]') FROM junkpurge_quarantine r),
'queue',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY id),'[]') FROM queue_jobs r),
'state',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY info_hash),'[]') FROM torrent_verdict_state r),
'events',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY id),'[]') FROM torrent_verdict_events r),
'evidence',(SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY id),'[]') FROM label_evidence r)
)::text`).Scan(&state))
	return state
}

func TestPostgresLegacyRestoreNormalizesQBPrivacyAuthority(t *testing.T) {
	for _, form := range []struct {
		name, source, left, right string
	}{
		{name: "exact", source: "qbittorrent"},
		{name: "padded", source: " \tqbittorrent\r\n", left: "\t ", right: " \n"},
		{name: "case", source: "QBITTORRENT"},
		{name: "unicode", source: "\u2003QbItToRrEnT\u00a0", left: "\u3000", right: "\u0085"},
	} {
		for _, category := range []string{"private", "bitgrab"} {
			t.Run(form.name+"/"+category, func(t *testing.T) {
				ctx, pool, vs := expiryIntegrationPool(t)
				h := expiryHash(451)
				seedExpirySnapshot(t, ctx, pool, vs, h)
				_, err := pool.Exec(ctx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength)
VALUES($2,'synthetic-privacy','fixture',encode($1::bytea,'hex'),$1,$3,now(),10)`, h, form.source, form.left+category+form.right)
				require.NoError(t, err)
				before := restorePrivacyState(t, ctx, pool)
				require.ErrorIs(t, RestoreQuarantined(ctx, pool, vs, nil, zap.NewNop().Sugar(), hex.EncodeToString(h)), cataloguerecovery.ErrProtected)
				require.Equal(t, before, restorePrivacyState(t, ctx, pool))
			})
		}
	}
}

func TestPostgresLegacyRestoreQBPrivacyRemainsSourceAndHashBound(t *testing.T) {
	for _, kind := range []string{"ordinary", "source_lookalike", "category_lookalike", "other_hash"} {
		t.Run(kind, func(t *testing.T) {
			ctx, pool, vs := expiryIntegrationPool(t)
			h := expiryHash(452)
			seedExpirySnapshot(t, ctx, pool, vs, h)
			if kind != "ordinary" {
				source, category, evidenceHash := "qbittorrent", "private", h
				switch kind {
				case "source_lookalike":
					source = "qbittorrent-copy"
				case "category_lookalike":
					category = "private-copy"
				case "other_hash":
					evidenceHash = expiryHash(453)
					source = "\u2003QBITTORRENT\u00a0"
				}
				_, err := pool.Exec(ctx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength)
VALUES($2,'synthetic-privacy','fixture',encode($1::bytea,'hex'),$1,$3,now(),10)`, evidenceHash, source, category)
				require.NoError(t, err)
			}
			require.NoError(t, RestoreQuarantined(ctx, pool, vs, nil, zap.NewNop().Sugar(), hex.EncodeToString(h)))
			state, events := expiryLedger(t, ctx, pool, h)
			require.Equal(t, verdicts.VerdictRestored, state)
			require.Equal(t, []string{verdicts.VerdictQuarantined, verdicts.VerdictRestored}, events)
			var restored bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM torrents WHERE info_hash=$1)
AND EXISTS(SELECT 1 FROM torrent_files WHERE info_hash=$1)
AND EXISTS(SELECT 1 FROM torrents_torrent_sources WHERE info_hash=$1 AND source='fixture' AND seeders=2)
AND EXISTS(SELECT 1 FROM queue_jobs WHERE queue='process_torrent')
AND NOT EXISTS(SELECT 1 FROM junkpurge_quarantine WHERE info_hash=$1)`, h).Scan(&restored))
			require.True(t, restored)
		})
	}
}

func TestPostgresLegacyRestoreWaitsForNormalizedQBPrivacyCommit(t *testing.T) {
	ctx, pool, vs := expiryIntegrationPool(t)
	h := expiryHash(454)
	seedExpirySnapshot(t, ctx, pool, vs, h)
	actor, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = actor.Rollback(ctx) })
	_, err = actor.Exec(ctx, `LOCK TABLE label_evidence IN ROW EXCLUSIVE MODE`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- RestoreQuarantined(ctx, pool, vs, nil, zap.NewNop().Sugar(), hex.EncodeToString(h)) }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock'
AND query LIKE '%LOCK TABLE label_evidence,torrent_canonical_labels%')`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond)
	_, err = actor.Exec(ctx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength)
VALUES($2,'synthetic-privacy','fixture',encode($1::bytea,'hex'),$1,$3,now(),10)`, h, "\u2003QBITTORRENT\u00a0", "\u3000BITGRAB\u0085")
	require.NoError(t, err)
	require.NoError(t, actor.Commit(ctx))
	before := restorePrivacyState(t, ctx, pool)
	require.ErrorIs(t, <-done, cataloguerecovery.ErrProtected)
	require.Equal(t, before, restorePrivacyState(t, ctx, pool))
}
