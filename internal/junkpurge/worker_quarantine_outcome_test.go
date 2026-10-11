package junkpurge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencercnorton/bitagent/internal/cataloguerecovery"
	"github.com/spencercnorton/bitagent/internal/telemetry/dualemit"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestQuarantineRecoveryHoldIsNotAnOperationalError(t *testing.T) {
	wasLegacy := dualemit.EmitLegacy
	dualemit.EmitLegacy = false
	t.Cleanup(func() { dualemit.EmitLegacy = wasLegacy })
	// Exercise the actual hard guard with a nonempty action and no database.
	rows, guardErr := quarantineJunk(context.Background(), nil, nil, [][]byte{bytes20(1)}, 0.99, 24*time.Hour, "synthetic-owner")
	require.ErrorIs(t, guardErr, cataloguerecovery.ErrDisabled)
	require.Empty(t, rows)
	for _, tc := range []struct {
		name        string
		err         error
		wantOutcome string
		wantErrors  float64
		wantLevel   zapcore.Level
	}{
		{"held", guardErr, cycleOutcomeQuarantineHeld, 0, zap.InfoLevel},
		{"wrapped_hold", fmt.Errorf("synthetic wrapper: %w", guardErr), cycleOutcomeQuarantineHeld, 0, zap.InfoLevel},
		{"real_error", errors.New("synthetic database failure"), cycleOutcomeQuarantineError, 1, zap.ErrorLevel},
		{"untyped_disabled_text", errors.New("recovery disabled"), cycleOutcomeQuarantineError, 1, zap.ErrorLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := NewMetrics()
			core, logs := observer.New(zap.InfoLevel)
			worker := &purgeWorker{metrics: metrics, logger: zap.New(core).Sugar()}
			outcome := worker.recordQuarantineError(context.Background(), nil, nil, tc.err)
			require.Equal(t, tc.wantOutcome, outcome)
			require.Equal(t, tc.wantErrors, testutil.ToFloat64(metrics.cycleErrorsTotal.WithLabelValues("quarantine")))
			require.Zero(t, testutil.ToFloat64(metrics.quarantinedTotal))
			require.Len(t, logs.All(), 1)
			require.Equal(t, tc.wantLevel, logs.All()[0].Level)
		})
	}
}

func TestPostgresQuarantineHoldRetainsRecordedJudgment(t *testing.T) {
	wasLegacy := dualemit.EmitLegacy
	dualemit.EmitLegacy = false
	t.Cleanup(func() { dualemit.EmitLegacy = wasLegacy })
	ctx, pool, _ := expiryIntegrationPool(t)
	hash := expiryHash(700)
	_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at)
VALUES($1,'Synthetic.Recorded.Release',4096,false,'single',1,now()-interval '48 hours',now())`, hash)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO junkpurge_judgments(info_hash,torrent_name,verdict,confidence,reason)
VALUES($1,'Synthetic.Recorded.Release','junk',0.99,$2)`, hash, judgmentReasonSyncPending)
	require.NoError(t, err)
	var before, after []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT (to_jsonb(j)-'reason')::text FROM junkpurge_judgments j WHERE info_hash=$1`, hash).Scan(&before))
	rows, guardErr := quarantineJunk(ctx, pool, nil, [][]byte{hash}, 0.99, 24*time.Hour, "synthetic-owner")
	require.ErrorIs(t, guardErr, cataloguerecovery.ErrDisabled)
	require.Empty(t, rows)
	metrics := NewMetrics()
	worker := &purgeWorker{metrics: metrics, logger: zap.NewNop().Sugar()}
	outcome := worker.recordQuarantineError(ctx, pool, [][]byte{hash}, guardErr)
	require.Equal(t, cycleOutcomeQuarantineHeld, outcome)
	var reason string
	require.NoError(t, pool.QueryRow(ctx, `SELECT (to_jsonb(j)-'reason')::text,reason FROM junkpurge_judgments j WHERE info_hash=$1`, hash).Scan(&after, &reason))
	require.Equal(t, before, after, "hold annotation changed the retained paid judgment")
	require.Equal(t, "sync_cycle:"+cycleOutcomeQuarantineHeld, reason)
	var raw, snapshots int
	require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*)FROM torrents WHERE info_hash=$1),(SELECT count(*)FROM junkpurge_quarantine WHERE info_hash=$1)`, hash).Scan(&raw, &snapshots))
	require.Equal(t, 1, raw)
	require.Zero(t, snapshots)
	require.Zero(t, testutil.ToFloat64(metrics.cycleErrorsTotal.WithLabelValues("quarantine")))
}
