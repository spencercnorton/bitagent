package junkpurge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencercnorton/bitagent/internal/lazy"
	migrationssql "github.com/spencercnorton/bitagent/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestBatchStoreIntegration exercises the SQL state machine against PostgreSQL
// when BITAGENT_TEST_POSTGRES_DSN is set. It is opt-in so ordinary unit tests
// stay hermetic; CI or a developer can point it at an ephemeral database.
func TestBatchStoreIntegration(t *testing.T) {
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to run PostgreSQL integration coverage")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("junkpurge_batch_test_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
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
		cleanup, cleanupErr := pgx.Connect(context.Background(), dsn)
		if cleanupErr == nil {
			_, _ = cleanup.Exec(
				context.Background(),
				`DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`,
			)
			_ = cleanup.Close(context.Background())
		}
	})

	_, err = pool.Exec(ctx, `
CREATE TABLE torrents (
  info_hash bytea PRIMARY KEY,
  name text NOT NULL,
  private boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);
CREATE TABLE torrent_files (
  info_hash bytea NOT NULL,
  "index" integer NOT NULL,
  path text NOT NULL,
  size bigint NOT NULL
);
CREATE TABLE torrent_contents (
  info_hash bytea NOT NULL REFERENCES torrents(info_hash) ON DELETE CASCADE,
  content_type text,
  content_id text,
  created_at timestamptz NOT NULL
);
CREATE TABLE torrent_canonical_labels (info_hash bytea PRIMARY KEY);
CREATE TABLE label_evidence (
  info_hash bytea,
  source text,
  category text
);
CREATE TABLE junkpurge_judgments (
  info_hash bytea PRIMARY KEY,
  verdict text NOT NULL,
  confidence real NOT NULL,
  reason text,
  torrent_name text,
  judged_at timestamptz NOT NULL DEFAULT now(),
  purged boolean NOT NULL DEFAULT false
);
CREATE TABLE junkpurge_quarantine (
  info_hash bytea PRIMARY KEY,
  torrent_name text NOT NULL,
  verdict text NOT NULL,
  confidence real NOT NULL,
  quarantined_at timestamptz NOT NULL DEFAULT now(),
  torrent_snapshot jsonb NOT NULL,
  files_snapshot jsonb,
  expired_at timestamptz
);`)
	require.NoError(t, err)

	migration, err := migrationssql.FS.ReadFile("00045_junkpurge_openai_batches.sql")
	require.NoError(t, err)
	up := strings.SplitN(string(migration), "-- +goose Down", 2)[0]
	_, err = pool.Exec(ctx, up)
	require.NoError(t, err)
	captureMigration, err := migrationssql.FS.ReadFile(
		"00046_llm_evaluation_capture.sql",
	)
	require.NoError(t, err)
	captureUp := strings.SplitN(
		string(captureMigration),
		"-- +goose Down",
		2,
	)[0]
	_, err = pool.Exec(ctx, captureUp)
	require.NoError(t, err)
	// candidateQuery excludes student-released candidates, so the table has to
	// exist for the query to plan at all.
	releasesMigration, err := migrationssql.FS.ReadFile(
		"00048_junkpurge_student_releases.sql",
	)
	require.NoError(t, err)
	releasesUp := strings.SplitN(string(releasesMigration), "-- +goose Down", 2)[0]
	_, err = pool.Exec(ctx, releasesUp)
	require.NoError(t, err)

	t.Run("isolated unavailable group preserves later evidence but blocks purge", func(t *testing.T) {
		hashes := [][]byte{bytes20(51), bytes20(52), bytes20(53), bytes20(54)}
		for index, hash := range hashes {
			insertCandidate(t, pool, hash, fmt.Sprintf("isolated outage %d", index))
		}
		cycleCfg := NewDefaultConfig()
		cycleCfg.Enabled = true
		cycleCfg.EnablePurge = true
		cycleCfg.BatchSize = len(hashes)
		cycleCfg.LLMNamesPerCall = 2
		cycleCfg.LLMUnavailableConsecutiveLimit = 2
		judge := &sequencedJudge{steps: []batchStep{
			{err: fmt.Errorf("%w: http 503", ErrLLMUnavailable)},
			{judgments: []Judgment{
				{Verdict: verdictJunk, Confidence: 0.99},
				{Verdict: verdictJunk, Confidence: 0.99},
			}},
		}}
		metrics := NewMetrics()
		w := &purgeWorker{
			cfg: cycleCfg,
			pool: lazy.New(func() (*pgxpool.Pool, error) {
				return pool, nil
			}),
			judge:           judge,
			metrics:         metrics,
			logger:          zap.NewNop().Sugar(),
			batchLeaseOwner: "sync-isolated-outage",
		}
		w.runCycle(ctx)

		var judgments, remaining, quarantined int
		require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*)
FROM junkpurge_judgments
WHERE info_hash=ANY($1) AND reason='sync_cycle:llm_unavailable'`, hashes).Scan(&judgments))
		require.Equal(t, 2, judgments,
			"the group after an isolated outage must remain usable evaluation evidence")
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM torrents WHERE info_hash=ANY($1)`, hashes,
		).Scan(&remaining))
		require.Equal(t, len(hashes), remaining,
			"any unavailable group must keep the whole destructive cycle blocked")
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM junkpurge_quarantine WHERE info_hash=ANY($1)`, hashes,
		).Scan(&quarantined))
		require.Zero(t, quarantined)

		_, err := pool.Exec(ctx, `
DELETE FROM junkpurge_judgments WHERE info_hash=ANY($1);
DELETE FROM torrents WHERE info_hash=ANY($1)`, hashes)
		require.NoError(t, err)
	})

	t.Run("canceled cycle finalizes completed group evidence", func(t *testing.T) {
		hashes := [][]byte{bytes20(55), bytes20(56), bytes20(57), bytes20(58)}
		for index, hash := range hashes {
			insertCandidate(t, pool, hash, fmt.Sprintf("canceled cycle %d", index))
		}
		cycleCfg := NewDefaultConfig()
		cycleCfg.Enabled = true
		cycleCfg.EnablePurge = true
		cycleCfg.BatchSize = len(hashes)
		cycleCfg.LLMNamesPerCall = 2
		runCtx, cancel := context.WithCancel(ctx)
		judge := &sequencedJudge{steps: []batchStep{
			{judgments: []Judgment{
				{Verdict: verdictRealMangled, Confidence: 0.99},
				{Verdict: verdictRealMangled, Confidence: 0.99},
			}},
			{before: cancel, err: context.Canceled},
		}}
		w := &purgeWorker{
			cfg: cycleCfg,
			pool: lazy.New(func() (*pgxpool.Pool, error) {
				return pool, nil
			}),
			judge:           judge,
			metrics:         NewMetrics(),
			logger:          zap.NewNop().Sugar(),
			batchLeaseOwner: "sync-canceled-cycle",
		}
		w.runCycle(runCtx)

		var judgments, remaining, quarantined int
		require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*)
FROM junkpurge_judgments
WHERE info_hash=ANY($1) AND reason='sync_cycle:canceled'`, hashes).Scan(&judgments))
		require.Equal(t, 2, judgments,
			"completed groups must not remain permanently marked pending after cancellation")
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM torrents WHERE info_hash=ANY($1)`, hashes,
		).Scan(&remaining))
		require.Equal(t, len(hashes), remaining)
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM junkpurge_quarantine WHERE info_hash=ANY($1)`, hashes,
		).Scan(&quarantined))
		require.Zero(t, quarantined)

		_, err := pool.Exec(ctx, `
DELETE FROM junkpurge_judgments WHERE info_hash=ANY($1);
DELETE FROM torrents WHERE info_hash=ANY($1)`, hashes)
		require.NoError(t, err)
	})

	t.Run("mid-fallback cancellation preserves paid prefix", func(t *testing.T) {
		hashes := [][]byte{bytes20(59), bytes20(60)}
		for index, hash := range hashes {
			insertCandidate(t, pool, hash, fmt.Sprintf("mid-fallback canceled cycle %d", index))
		}
		cycleCfg := NewDefaultConfig()
		cycleCfg.Enabled = true
		cycleCfg.EnablePurge = true
		cycleCfg.BatchSize = len(hashes)
		cycleCfg.LLMNamesPerCall = 2
		runCtx, cancel := context.WithCancel(ctx)
		judge := &midFallbackCancelJudge{
			cancel: cancel,
			judgment: Judgment{
				Verdict: verdictJunk, Confidence: 0.99,
			},
		}
		w := &purgeWorker{
			cfg: cycleCfg,
			pool: lazy.New(func() (*pgxpool.Pool, error) {
				return pool, nil
			}),
			judge:           judge,
			metrics:         NewMetrics(),
			logger:          zap.NewNop().Sugar(),
			batchLeaseOwner: "sync-mid-fallback-canceled-cycle",
		}
		w.runCycle(runCtx)

		var judgments, remaining, quarantined int
		require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*)
FROM junkpurge_judgments
WHERE info_hash=ANY($1) AND reason='sync_cycle:canceled'`, hashes).Scan(&judgments))
		require.Equal(t, 1, judgments,
			"the paid single-name prefix must be recorded and finalized after cancellation")
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM torrents WHERE info_hash=ANY($1)`, hashes,
		).Scan(&remaining))
		require.Equal(t, len(hashes), remaining,
			"cancellation must keep the entire destructive cycle blocked")
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM junkpurge_quarantine WHERE info_hash=ANY($1)`, hashes,
		).Scan(&quarantined))
		require.Zero(t, quarantined)

		_, err := pool.Exec(ctx, `
DELETE FROM junkpurge_judgments WHERE info_hash=ANY($1);
DELETE FROM torrents WHERE info_hash=ANY($1)`, hashes)
		require.NoError(t, err)
	})

	t.Run("capture-only candidate query executes under prepared protocol", func(t *testing.T) {
		preparedCfg, parseErr := pgxpool.ParseConfig(dsn)
		require.NoError(t, parseErr)
		preparedCfg.ConnConfig.RuntimeParams["search_path"] = schema
		preparedCfg.ConnConfig.DefaultQueryExecMode =
			pgx.QueryExecModeCacheStatement
		preparedPool, poolErr := pgxpool.NewWithConfig(ctx, preparedCfg)
		require.NoError(t, poolErr)
		t.Cleanup(preparedPool.Close)

		captureHash := bytes20(30)
		insertCandidate(t, pool, captureHash, "capture-only prepared query")
		captureCfg := NewDefaultConfig()
		captureCfg.BatchSize = 1
		candidates, queryErr := findCaptureCandidates(
			ctx,
			preparedPool,
			captureCfg,
		)
		require.NoError(t, queryErr)
		require.Len(t, candidates, 1)
		require.Equal(t, captureHash, candidates[0].infoHash)
		_, queryErr = pool.Exec(
			ctx,
			`DELETE FROM torrents WHERE info_hash=$1`,
			captureHash,
		)
		require.NoError(t, queryErr)
	})

	t.Run("capture-only preflight refuses active Batch work", func(t *testing.T) {
		activeHash := bytes20(35)
		insertCandidate(t, pool, activeHash, "active Batch preflight")
		activeCfg := NewDefaultConfig()
		activeCfg.BatchSize = 1
		activeCfg.LLMBatchMaxInFlight = 1
		activeRun, createErr := createBatchRun(ctx, pool, activeCfg)
		require.NoError(t, createErr)
		require.NotNil(t, activeRun)

		preflightErr := requireCaptureOnlyBatchIdle(ctx, pool)
		require.ErrorContains(
			t,
			preflightErr,
			"requires zero active Batch runs; found 1",
		)
		_, createErr = pool.Exec(ctx, `
UPDATE junkpurge_batch_items
SET state='abandoned'
WHERE run_id=$1;
UPDATE junkpurge_batch_runs
SET state='failed', updated_at=now()
WHERE id=$1;
DELETE FROM torrents
WHERE info_hash=$2`,
			activeRun.ID,
			activeHash,
		)
		require.NoError(t, createErr)
		require.NoError(t, requireCaptureOnlyBatchIdle(ctx, pool))
	})

	topUpJunkA := bytes20(31)
	topUpJunkB := bytes20(32)
	topUpRealA := bytes20(33)
	topUpRealB := bytes20(34)
	for _, fixture := range []struct {
		hash    []byte
		name    string
		verdict string
	}{
		{topUpJunkA, "top-up junk a", "junk"},
		{topUpJunkB, "top-up junk b", "junk"},
		{topUpRealA, "top-up real a", "real_mangled"},
		{topUpRealB, "top-up real b", "real_absent"},
	} {
		insertCandidate(t, pool, fixture.hash, fixture.name)
		_, err = pool.Exec(ctx, `
INSERT INTO junkpurge_judgments (
  info_hash, verdict, confidence, torrent_name, judged_at, purged
) VALUES ($1,$2,0.99,$3,now()-interval '120 days',false)`,
			fixture.hash,
			fixture.verdict,
			fixture.name,
		)
		require.NoError(t, err)
	}
	topUpConfig := NewDefaultConfig()
	topUpConfig.BatchSize = 3
	topUps, err := findCaptureSafetyTopUpCandidates(
		ctx,
		pool,
		topUpConfig,
	)
	require.NoError(t, err)
	require.Len(t, topUps, 3)
	var topUpJunkCount, topUpRealCount int
	for _, topUp := range topUps {
		switch {
		case bytes.Equal(topUp.infoHash, topUpJunkA),
			bytes.Equal(topUp.infoHash, topUpJunkB):
			topUpJunkCount++
		case bytes.Equal(topUp.infoHash, topUpRealA),
			bytes.Equal(topUp.infoHash, topUpRealB):
			topUpRealCount++
		default:
			require.Failf(
				t,
				"unexpected top-up",
				"info_hash=%x",
				topUp.infoHash,
			)
		}
	}
	require.Equal(t, 2, topUpJunkCount)
	require.Equal(t, 1, topUpRealCount)
	_, err = pool.Exec(ctx, `
DELETE FROM junkpurge_judgments
WHERE info_hash=ANY($1);
DELETE FROM torrents
WHERE info_hash=ANY($1)`,
		[][]byte{topUpJunkA, topUpJunkB, topUpRealA, topUpRealB},
	)
	require.NoError(t, err)

	hashA := bytes20(1)
	hashB := bytes20(2)
	hashPrivate := bytes20(21)
	insertCandidate(t, pool, hashA, "junk title")
	insertCandidate(t, pool, hashB, "real title")
	insertCandidate(t, pool, hashPrivate, "private title")
	_, err = pool.Exec(
		ctx,
		`UPDATE torrents SET private=true WHERE info_hash=$1`,
		hashPrivate,
	)
	require.NoError(t, err)

	workerCfg := NewDefaultConfig()
	workerCfg.BatchSize = 3
	workerCfg.LLMBatchMaxInFlight = 2
	workerCfg.MaxJunkRate = 0.9
	workerCfg.LLMModel = "gpt-test"
	workerCfg.LLMBatchCompletionWindow = "24h"

	run, err := createBatchRun(ctx, pool, workerCfg)
	require.NoError(t, err)
	require.NotNil(t, run)
	require.Equal(t, 2, run.ItemCount)
	var privateBatchItems int
	require.NoError(t, pool.QueryRow(
		ctx,
		`SELECT count(*) FROM junkpurge_batch_items WHERE info_hash=$1`,
		hashPrivate,
	).Scan(&privateBatchItems))
	require.Zero(t, privateBatchItems)
	privateOwned, err := claimSyncCandidate(
		ctx, pool, hashPrivate, "private title", "sync-private",
		time.Hour, workerCfg,
	)
	require.NoError(t, err)
	require.False(t, privateOwned)

	client := newOpenAIBatchClient(workerCfg)
	attempt, err := prepareBatchAttempt(
		ctx, pool, *run, client.BuildInput,
	)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	require.NotEmpty(t, attempt.Payload)
	lockA, locked, err := acquireBatchAttemptAdvisoryLock(ctx, pool, attempt.ID)
	require.NoError(t, err)
	require.True(t, locked)
	lockB, locked, err := acquireBatchAttemptAdvisoryLock(ctx, pool, attempt.ID)
	require.NoError(t, err)
	if locked && lockB != nil {
		_ = releaseBatchAttemptAdvisoryLock(ctx, lockB, attempt.ID)
	}
	require.NoError(t, releaseBatchAttemptAdvisoryLock(ctx, lockA, attempt.ID))
	require.False(t, locked)
	require.Nil(t, lockB)
	lockB, locked, err = acquireBatchAttemptAdvisoryLock(ctx, pool, attempt.ID)
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, releaseBatchAttemptAdvisoryLock(ctx, lockB, attempt.ID))

	claimed, err := claimBatchAttempt(ctx, pool, attempt.ID, "worker-a", time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = claimBatchAttempt(ctx, pool, attempt.ID, "worker-b", time.Minute)
	require.NoError(t, err)
	require.False(t, claimed)
	require.NoError(t, releaseBatchAttempt(ctx, pool, attempt.ID, "worker-a"))
	claimed, err = claimBatchAttempt(ctx, pool, attempt.ID, "worker-b", time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, updateAttemptProvider(
		ctx, pool, attempt.ID, "completed",
		"output-stable", "error-stable", 2, 2, 0, true, "worker-b",
	))
	require.NoError(t, updateAttemptProvider(
		ctx, pool, attempt.ID, "completed",
		"", "", 2, 2, 0, true, "worker-b",
	))
	reloadedAttempt, err := loadBatchAttempt(ctx, pool, attempt.ID)
	require.NoError(t, err)
	require.Equal(t, "output-stable", reloadedAttempt.OutputFileID)
	require.Equal(t, "error-stable", reloadedAttempt.ErrorFileID)
	require.NoError(t, releaseBatchAttempt(ctx, pool, attempt.ID, "worker-b"))

	items, err := loadAttemptItems(ctx, pool, attempt.ID)
	require.NoError(t, err)
	require.Len(t, items, 2)
	results := []batchItemResult{
		{
			CustomID: items[1].CustomID, Succeeded: true,
			Judgment:       Judgment{Verdict: verdictRealMangled, Confidence: 0.95},
			ResponseStatus: 200, Usage: tokenUsage{Input: 11, Output: 2},
		},
		{
			CustomID: items[0].CustomID, Succeeded: true,
			Judgment:       Judgment{Verdict: verdictJunk, Confidence: 0.96},
			ResponseStatus: 200, Usage: tokenUsage{Input: 10, Output: 2},
		},
	}
	claimed, err = claimBatchAttempt(ctx, pool, attempt.ID, "ingest-a", time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	ingested, err := ingestBatchAttempt(
		ctx, pool, *attempt, "completed", results, "ingest-a",
	)
	require.NoError(t, err)
	require.NoError(t, releaseBatchAttempt(ctx, pool, attempt.ID, "ingest-a"))
	require.Equal(t, 2, ingested.Succeeded)
	require.Equal(t, runStateFinalizing, ingested.RunState)

	finalizing, err := listFinalizingRuns(ctx, pool, 10)
	require.NoError(t, err)
	require.Len(t, finalizing, 1)
	finalized, err := finalizeBatchRun(ctx, pool, finalizing[0], false)
	require.NoError(t, err)
	require.Equal(t, runStateDryRun, finalized.State)
	require.Equal(t, 2, finalized.Judged)
	require.Equal(t, 1, finalized.Junk)
	require.Equal(t, 1, finalized.WouldDelete)
	require.Empty(t, finalized.QuarantinedHashes)

	var judgments, torrents int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM junkpurge_judgments`).Scan(&judgments))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrents`).Scan(&torrents))
	require.Equal(t, 2, judgments)
	require.Equal(t, 3, torrents)

	// Reservation is not an egress authorization. Native privacy and qB
	// private/bitgrab evidence can arrive while a pending cohort waits for
	// payload construction; the final DB-locked gate must abandon those items
	// before their stored torrent names enter JSONL.
	hashNativePrivate := bytes20(22)
	hashQBPrivate := bytes20(23)
	hashQBBitgrab := bytes20(24)
	hashPublicControl := bytes20(25)
	const (
		nativePrivateName = "native-private-after-reservation-secret"
		qbPrivateName     = "qb-private-after-reservation-secret"
		qbBitgrabName     = "qb-bitgrab-after-reservation-secret"
		publicControlName = "public-pre-upload-control"
	)
	insertCandidate(t, pool, hashNativePrivate, nativePrivateName)
	insertCandidate(t, pool, hashQBPrivate, qbPrivateName)
	insertCandidate(t, pool, hashQBBitgrab, qbBitgrabName)
	insertCandidate(t, pool, hashPublicControl, publicControlName)
	workerCfg.BatchSize = 4
	workerCfg.LLMBatchMaxInFlight = 2
	privacyRun, err := createBatchRun(ctx, pool, workerCfg)
	require.NoError(t, err)
	require.NotNil(t, privacyRun)
	require.Equal(t, 4, privacyRun.ItemCount)

	_, err = pool.Exec(ctx, `
UPDATE torrents SET private=true WHERE info_hash=$1;
INSERT INTO label_evidence (info_hash,source,category)
VALUES ($2,'qbittorrent','private'),($3,'qbittorrent','bitgrab')`,
		hashNativePrivate, hashQBPrivate, hashQBBitgrab,
	)
	require.NoError(t, err)

	privacyAttempt, err := prepareBatchAttempt(
		ctx, pool, *privacyRun, client.BuildInput,
	)
	require.NoError(t, err)
	require.NotNil(t, privacyAttempt)
	require.Equal(t, 1, privacyAttempt.ItemCount)
	require.True(t, bytes.Contains(
		privacyAttempt.Payload,
		[]byte(publicControlName),
	))
	for _, blockedName := range []string{
		nativePrivateName,
		qbPrivateName,
		qbBitgrabName,
	} {
		require.Falsef(
			t,
			bytes.Contains(privacyAttempt.Payload, []byte(blockedName)),
			"blocked torrent name %q entered provider JSONL",
			blockedName,
		)
	}
	var excluded, adjustedItemCount int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*)
FROM junkpurge_batch_items
WHERE run_id=$1
  AND info_hash IN ($2,$3,$4)
  AND state='abandoned'
  AND error_code='pre_upload_ineligible'`,
		privacyRun.ID, hashNativePrivate, hashQBPrivate, hashQBBitgrab,
	).Scan(&excluded))
	require.Equal(t, 3, excluded)
	require.NoError(t, pool.QueryRow(ctx, `
SELECT item_count FROM junkpurge_batch_runs WHERE id=$1`,
		privacyRun.ID,
	).Scan(&adjustedItemCount))
	require.Equal(t, 1, adjustedItemCount)

	privacyItems, err := loadAttemptItems(ctx, pool, privacyAttempt.ID)
	require.NoError(t, err)
	require.Len(t, privacyItems, 1)
	require.Equal(t, hashPublicControl, privacyItems[0].InfoHash)
	claimed, err = claimBatchAttempt(
		ctx, pool, privacyAttempt.ID, "ingest-privacy-control", time.Minute,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	privacySummary, err := ingestBatchAttempt(
		ctx,
		pool,
		*privacyAttempt,
		"completed",
		[]batchItemResult{{
			CustomID:        privacyItems[0].CustomID,
			Succeeded:       true,
			Judgment:        Judgment{Verdict: verdictRealMangled, Confidence: 0.99},
			ResponseStatus:  200,
			ProviderRequest: "privacy-control",
		}},
		"ingest-privacy-control",
	)
	require.NoError(t, err)
	require.Equal(t, runStateFinalizing, privacySummary.RunState)
	require.NoError(t, releaseBatchAttempt(
		ctx, pool, privacyAttempt.ID, "ingest-privacy-control",
	))
	finalizing, err = listFinalizingRuns(ctx, pool, 10)
	require.NoError(t, err)
	require.Len(t, finalizing, 1)
	require.Equal(t, privacyRun.ID, finalizing[0].ID)
	finalized, err = finalizeBatchRun(ctx, pool, finalizing[0], false)
	require.NoError(t, err)
	require.Equal(t, runStateDryRun, finalized.State)
	require.Equal(t, 1, finalized.Judged)

	// The same gate applies to a retryable item reserved by an older attempt.
	// Once qB private evidence appears, no replacement JSONL is constructed and
	// an all-excluded run advances to finalizing instead of stranding active.
	hashRetryPrivate := bytes20(26)
	const retryPrivateName = "retry-became-private-secret"
	insertCandidate(t, pool, hashRetryPrivate, retryPrivateName)
	workerCfg.BatchSize = 1
	workerCfg.LLMBatchMaxAttempts = 2
	retryPrivacyRun, err := createBatchRun(ctx, pool, workerCfg)
	require.NoError(t, err)
	require.NotNil(t, retryPrivacyRun)
	retryPrivacyAttempt, err := prepareBatchAttempt(
		ctx, pool, *retryPrivacyRun, client.BuildInput,
	)
	require.NoError(t, err)
	require.NotNil(t, retryPrivacyAttempt)
	retryPrivacyItems, err := loadAttemptItems(
		ctx, pool, retryPrivacyAttempt.ID,
	)
	require.NoError(t, err)
	require.Len(t, retryPrivacyItems, 1)
	claimed, err = claimBatchAttempt(
		ctx, pool, retryPrivacyAttempt.ID, "ingest-retry-privacy", time.Minute,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	retryPrivacySummary, err := ingestBatchAttempt(
		ctx,
		pool,
		*retryPrivacyAttempt,
		"expired",
		[]batchItemResult{{
			CustomID:       retryPrivacyItems[0].CustomID,
			Retryable:      true,
			ResponseStatus: 429,
			ErrorCode:      "rate_limit_exceeded",
		}},
		"ingest-retry-privacy",
	)
	require.NoError(t, err)
	require.Equal(t, runStateActive, retryPrivacySummary.RunState)
	require.NoError(t, releaseBatchAttempt(
		ctx, pool, retryPrivacyAttempt.ID, "ingest-retry-privacy",
	))
	_, err = pool.Exec(ctx, `
INSERT INTO label_evidence (info_hash,source,category)
VALUES ($1,'qbittorrent','bitgrab')`, hashRetryPrivate)
	require.NoError(t, err)
	replacementBuilt := false
	retryPrivacyReplacement, err := prepareBatchAttempt(
		ctx,
		pool,
		*retryPrivacyRun,
		func(batchRun, int, []batchItem) ([]byte, string, string, error) {
			replacementBuilt = true
			return nil, "", "", fmt.Errorf(
				"replacement payload must not be built after privacy evidence",
			)
		},
	)
	require.NoError(t, err)
	require.Nil(t, retryPrivacyReplacement)
	require.False(t, replacementBuilt)
	var retryPrivacyState, retryPrivacyItemState string
	require.NoError(t, pool.QueryRow(ctx, `
SELECT state FROM junkpurge_batch_runs WHERE id=$1`,
		retryPrivacyRun.ID,
	).Scan(&retryPrivacyState))
	require.Equal(t, runStateFinalizing, retryPrivacyState)
	require.NoError(t, pool.QueryRow(ctx, `
SELECT state FROM junkpurge_batch_items
WHERE run_id=$1 AND info_hash=$2`,
		retryPrivacyRun.ID, hashRetryPrivate,
	).Scan(&retryPrivacyItemState))
	require.Equal(t, itemStateAbandoned, retryPrivacyItemState)
	finalizing, err = listFinalizingRuns(ctx, pool, 10)
	require.NoError(t, err)
	require.Len(t, finalizing, 1)
	require.Equal(t, retryPrivacyRun.ID, finalizing[0].ID)
	finalized, err = finalizeBatchRun(ctx, pool, finalizing[0], false)
	require.NoError(t, err)
	require.Equal(t, runStateDryRun, finalized.State)
	require.Zero(t, finalized.Judged)

	// Deployment can inherit manifests persisted by an older binary. Recheck
	// those bytes at both provider POST boundaries: prepared/uploading must
	// never upload, while uploaded must never create a Batch and its known
	// provider file is cleaned up.
	legacyCases := []struct {
		name       string
		hash       []byte
		state      string
		inputFile  string
		addPrivacy func()
	}{
		{
			name:  "prepared native-private",
			hash:  bytes20(27),
			state: "prepared",
			addPrivacy: func() {
				_, updateErr := pool.Exec(ctx,
					`UPDATE torrents SET private=true WHERE info_hash=$1`,
					bytes20(27),
				)
				require.NoError(t, updateErr)
			},
		},
		{
			name:  "uploading qB-private",
			hash:  bytes20(28),
			state: "uploading",
			addPrivacy: func() {
				_, insertErr := pool.Exec(ctx, `
INSERT INTO label_evidence (info_hash,source,category)
VALUES ($1,'qbittorrent','private')`, bytes20(28))
				require.NoError(t, insertErr)
			},
		},
		{
			name:      "uploaded qB-bitgrab",
			hash:      bytes20(29),
			state:     "uploaded",
			inputFile: "legacy-private-input-file",
			addPrivacy: func() {
				_, insertErr := pool.Exec(ctx, `
INSERT INTO label_evidence (info_hash,source,category)
VALUES ($1,'qbittorrent','bitgrab')`, bytes20(29))
				require.NoError(t, insertErr)
			},
		},
	}
	for _, testCase := range legacyCases {
		t.Run("legacy "+testCase.name, func(t *testing.T) {
			torrentName := "legacy-" + testCase.name + "-secret"
			insertCandidate(t, pool, testCase.hash, torrentName)
			workerCfg.BatchSize = 1
			workerCfg.LLMBatchMaxAttempts = 3
			legacyRun, createErr := createBatchRun(ctx, pool, workerCfg)
			require.NoError(t, createErr)
			require.NotNil(t, legacyRun)
			legacyAttempt, prepareErr := prepareBatchAttempt(
				ctx, pool, *legacyRun, client.BuildInput,
			)
			require.NoError(t, prepareErr)
			require.NotNil(t, legacyAttempt)

			switch testCase.state {
			case "prepared":
			case "uploading":
				_, prepareErr = pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state='uploading',
    submission_attempted_at=now()-interval '2 hours'
WHERE id=$1`, legacyAttempt.ID)
				require.NoError(t, prepareErr)
			case "uploaded":
				_, prepareErr = pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state='uploaded', input_file_id=$2
WHERE id=$1`, legacyAttempt.ID, testCase.inputFile)
				require.NoError(t, prepareErr)
			default:
				t.Fatalf("unsupported legacy state %q", testCase.state)
			}
			testCase.addPrivacy()

			provider := &privacyBoundaryBatchClient{
				baseURL: legacyAttempt.ProviderBaseURL,
			}
			testWorker := &purgeWorker{
				cfg:             workerCfg,
				metrics:         NewMetrics(),
				logger:          zap.NewNop().Sugar(),
				batchLeaseOwner: "privacy-boundary-" + testCase.state,
			}
			require.NoError(t, testWorker.reconcileBatchAttempt(
				ctx, pool, provider, *legacyAttempt, true,
			))
			require.Zero(t, provider.uploadCalls)
			require.Zero(t, provider.createCalls)
			if testCase.state == "uploaded" {
				require.Equal(t, 1, provider.findBatchCalls)
				require.Equal(t, []string{testCase.inputFile}, provider.deletedFiles)
			} else {
				require.Equal(t, 1, provider.findInputCalls)
				require.Empty(t, provider.deletedFiles)
			}

			var persistedAttemptState, persistedRunState, persistedItemState string
			var ingested bool
			require.NoError(t, pool.QueryRow(ctx, `
SELECT state, ingested_at IS NOT NULL
FROM junkpurge_batch_attempts
WHERE id=$1`, legacyAttempt.ID,
			).Scan(&persistedAttemptState, &ingested))
			require.Equal(t, runStateFailed, persistedAttemptState)
			require.True(t, ingested)
			require.NoError(t, pool.QueryRow(ctx, `
SELECT state FROM junkpurge_batch_runs WHERE id=$1`, legacyRun.ID,
			).Scan(&persistedRunState))
			require.Equal(t, runStateFailed, persistedRunState)
			require.NoError(t, pool.QueryRow(ctx, `
SELECT state FROM junkpurge_batch_items
WHERE run_id=$1 AND info_hash=$2`, legacyRun.ID, testCase.hash,
			).Scan(&persistedItemState))
			require.Equal(t, itemStateAbandoned, persistedItemState)
		})
	}

	// A privacy-blocked upload whose provider DELETE fails must stay eligible
	// for cleanup. Losing it with the terminal transition would leave newly
	// private data hosted by the provider indefinitely.
	t.Run("privacy-blocked input cleanup is durably retried", func(t *testing.T) {
		hash := bytes20(40)
		const inputFile = "durable-cleanup-input-file"
		insertCandidate(t, pool, hash, "durable-cleanup-secret")
		workerCfg.BatchSize = 1
		workerCfg.LLMBatchMaxAttempts = 3
		run, createErr := createBatchRun(ctx, pool, workerCfg)
		require.NoError(t, createErr)
		require.NotNil(t, run)
		attempt, prepareErr := prepareBatchAttempt(
			ctx, pool, *run, client.BuildInput,
		)
		require.NoError(t, prepareErr)
		require.NotNil(t, attempt)
		_, execErr := pool.Exec(ctx, `
UPDATE junkpurge_batch_attempts
SET state='uploaded', input_file_id=$2
WHERE id=$1`, attempt.ID, inputFile)
		require.NoError(t, execErr)
		_, execErr = pool.Exec(ctx, `
INSERT INTO label_evidence (info_hash,source,category)
VALUES ($1,'qbittorrent','private')`, hash)
		require.NoError(t, execErr)

		failing := &privacyBoundaryBatchClient{
			baseURL:     attempt.ProviderBaseURL,
			failDeletes: true,
		}
		testWorker := &purgeWorker{
			cfg:             workerCfg,
			metrics:         NewMetrics(),
			logger:          zap.NewNop().Sugar(),
			batchLeaseOwner: "durable-cleanup",
		}
		require.NoError(t, testWorker.reconcileBatchAttempt(
			ctx, pool, failing, *attempt, true,
		))
		require.Zero(t, failing.createCalls)
		require.Equal(t, []string{inputFile}, failing.deletedFiles)

		// The delete failed, so the id survives and the attempt is still listed.
		var retained string
		require.NoError(t, pool.QueryRow(ctx, `
SELECT coalesce(input_file_id,'') FROM junkpurge_batch_attempts
WHERE id=$1`, attempt.ID,
		).Scan(&retained))
		require.Equal(t, inputFile, retained)
		pending, listErr := listBatchAttemptsPendingInputCleanup(ctx, pool, 100)
		require.NoError(t, listErr)
		require.Contains(t, attemptIDs(pending), attempt.ID)

		// A later cycle with a healthy provider deletes and journals it.
		healthy := &privacyBoundaryBatchClient{baseURL: attempt.ProviderBaseURL}
		testWorker.cleanupSettledBatchInputFiles(ctx, pool, healthy)
		require.Equal(t, []string{inputFile}, healthy.deletedFiles)
		require.NoError(t, pool.QueryRow(ctx, `
SELECT coalesce(input_file_id,'') FROM junkpurge_batch_attempts
WHERE id=$1`, attempt.ID,
		).Scan(&retained))
		require.Empty(t, retained)
		pending, listErr = listBatchAttemptsPendingInputCleanup(ctx, pool, 100)
		require.NoError(t, listErr)
		require.NotContains(t, attemptIDs(pending), attempt.ID)

		// Cleanup is idempotent: nothing left to delete on the next cycle.
		again := &privacyBoundaryBatchClient{baseURL: attempt.ProviderBaseURL}
		testWorker.cleanupSettledBatchInputFiles(ctx, pool, again)
		require.Empty(t, again.deletedFiles)
	})

	// Finalization locks content rows before computing the dry-run drop list.
	// A match transaction already in flight must become visible after the
	// lock wait, so the stale LLM result is neither recorded nor counted as a
	// would-delete.
	hashM := bytes20(13)
	insertCandidate(t, pool, hashM, "matched during batch finalize")
	workerCfg.BatchSize = 1
	workerCfg.MaxJunkRate = 1
	staleRun, err := createBatchRun(ctx, pool, workerCfg)
	require.NoError(t, err)
	require.NotNil(t, staleRun)
	staleAttempt, err := prepareBatchAttempt(
		ctx, pool, *staleRun, client.BuildInput,
	)
	require.NoError(t, err)
	staleItems, err := loadAttemptItems(ctx, pool, staleAttempt.ID)
	require.NoError(t, err)
	require.Len(t, staleItems, 1)
	claimed, err = claimBatchAttempt(
		ctx, pool, staleAttempt.ID, "ingest-stale", time.Minute,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = ingestBatchAttempt(ctx, pool, *staleAttempt, "completed",
		[]batchItemResult{{
			CustomID:  staleItems[0].CustomID,
			Succeeded: true,
			Judgment: Judgment{
				Verdict: verdictJunk, Confidence: 0.99,
			},
			ResponseStatus: 200,
		}}, "ingest-stale")
	require.NoError(t, err)
	require.NoError(t, releaseBatchAttempt(
		ctx, pool, staleAttempt.ID, "ingest-stale",
	))

	matchTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = matchTx.Exec(ctx,
		`UPDATE torrent_contents SET content_id='tmdb-late' WHERE info_hash=$1`,
		hashM,
	)
	require.NoError(t, err)
	type finalizeResult struct {
		summary batchFinalizeSummary
		err     error
	}
	finalizeCh := make(chan finalizeResult, 1)
	go func() {
		finalizeCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		summary, finalizeErr := finalizeBatchRun(
			finalizeCtx, pool, *staleRun, false,
		)
		finalizeCh <- finalizeResult{summary: summary, err: finalizeErr}
	}()
	select {
	case premature := <-finalizeCh:
		_ = matchTx.Rollback(ctx)
		t.Fatalf("finalization did not wait for content lock: %+v", premature)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, matchTx.Commit(ctx))
	select {
	case result := <-finalizeCh:
		require.NoError(t, result.err)
		require.Equal(t, runStateDryRun, result.summary.State)
		require.Zero(t, result.summary.WouldDelete)
	case <-time.After(5 * time.Second):
		t.Fatal("finalization did not resume after content update commit")
	}
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM junkpurge_judgments WHERE info_hash=$1`,
		hashM,
	).Scan(&judgments))
	require.Zero(t, judgments)

	// Live settlement uses the same transaction for judgment, quarantine
	// snapshot, torrent deletion, item application, and run finalization.
	hashC := bytes20(3)
	insertCandidate(t, pool, hashC, "obvious junk")
	workerCfg.BatchSize = 1
	workerCfg.EnablePurge = true
	workerCfg.MaxJunkRate = 1
	liveRun, err := createBatchRun(ctx, pool, workerCfg)
	require.NoError(t, err)
	require.NotNil(t, liveRun)
	liveAttempt, err := prepareBatchAttempt(
		ctx, pool, *liveRun, client.BuildInput,
	)
	require.NoError(t, err)
	liveItems, err := loadAttemptItems(ctx, pool, liveAttempt.ID)
	require.NoError(t, err)
	claimed, err = claimBatchAttempt(ctx, pool, liveAttempt.ID, "ingest-b", time.Minute)
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = ingestBatchAttempt(ctx, pool, *liveAttempt, "completed",
		[]batchItemResult{{
			CustomID: liveItems[0].CustomID, Succeeded: true,
			Judgment:       Judgment{Verdict: verdictJunk, Confidence: 0.99},
			ResponseStatus: 200,
		}}, "ingest-b")
	require.NoError(t, err)
	require.NoError(t, releaseBatchAttempt(ctx, pool, liveAttempt.ID, "ingest-b"))
	finalizing, err = listFinalizingRuns(ctx, pool, 10)
	require.NoError(t, err)
	require.Len(t, finalizing, 1)
	finalized, err = finalizeBatchRun(ctx, pool, finalizing[0], true)
	require.NoError(t, err)
	require.Equal(t, runStateCompleted, finalized.State)
	require.Equal(t, [][]byte{hashC}, finalized.QuarantinedHashes)

	var remaining, quarantined, purged int
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM torrents WHERE info_hash=$1`, hashC,
	).Scan(&remaining))
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM junkpurge_quarantine WHERE info_hash=$1`, hashC,
	).Scan(&quarantined))
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM junkpurge_judgments WHERE info_hash=$1 AND purged`, hashC,
	).Scan(&purged))
	require.Zero(t, remaining)
	require.Equal(t, 1, quarantined)
	require.Equal(t, 1, purged)

	// Two replicas racing the final allowed attempt must create exactly one;
	// the loser rechecks for A's unsettled attempt under the run lock instead
	// of declaring retry exhaustion and abandoning paid work.
	hashD := bytes20(4)
	insertCandidate(t, pool, hashD, "attempt race")
	workerCfg.EnablePurge = false
	workerCfg.LLMBatchMaxAttempts = 1
	raceRun, err := createBatchRun(ctx, pool, workerCfg)
	require.NoError(t, err)
	require.NotNil(t, raceRun)
	type prepareResult struct {
		attempt *batchAttempt
		err     error
	}
	start := make(chan struct{})
	resultsCh := make(chan prepareResult, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			attempt, err := prepareBatchAttempt(
				context.Background(), pool, *raceRun, client.BuildInput,
			)
			resultsCh <- prepareResult{attempt: attempt, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(resultsCh)
	prepared := 0
	for result := range resultsCh {
		require.NoError(t, result.err)
		if result.attempt != nil {
			prepared++
		}
	}
	require.Equal(t, 1, prepared)
	var attemptCount int
	var runState string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM junkpurge_batch_attempts WHERE run_id=$1`,
		raceRun.ID,
	).Scan(&attemptCount))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT state FROM junkpurge_batch_runs WHERE id=$1`,
		raceRun.ID,
	).Scan(&runState))
	require.Equal(t, 1, attemptCount)
	require.Equal(t, runStateActive, runState)

	// A cancelled provider job is categorically failed even if its output
	// happens to contain valid successful rows. It must never advance to
	// finalization or write a destructive judgment.
	hashG := bytes20(7)
	insertCandidate(t, pool, hashG, "cancelled output")
	workerCfg.LLMBatchMaxInFlight = 3
	workerCfg.LLMBatchMaxAttempts = 3
	cancelledRun, err := createBatchRun(ctx, pool, workerCfg)
	require.NoError(t, err)
	require.NotNil(t, cancelledRun)
	cancelledAttempt, err := prepareBatchAttempt(
		ctx, pool, *cancelledRun, client.BuildInput,
	)
	require.NoError(t, err)
	cancelledItems, err := loadAttemptItems(ctx, pool, cancelledAttempt.ID)
	require.NoError(t, err)
	require.Len(t, cancelledItems, 1)
	claimed, err = claimBatchAttempt(
		ctx, pool, cancelledAttempt.ID, "ingest-cancelled", time.Minute,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	cancelledSummary, err := ingestBatchAttempt(
		ctx, pool, *cancelledAttempt, "cancelled",
		[]batchItemResult{{
			CustomID:  cancelledItems[0].CustomID,
			Succeeded: true,
			Judgment: Judgment{
				Verdict: verdictJunk, Confidence: 0.99,
			},
			ResponseStatus: 200,
		}},
		"ingest-cancelled",
	)
	require.NoError(t, err)
	require.Equal(t, runStateFailed, cancelledSummary.RunState)
	require.NoError(t, releaseBatchAttempt(
		ctx, pool, cancelledAttempt.ID, "ingest-cancelled",
	))
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT state FROM junkpurge_batch_runs WHERE id=$1`,
		cancelledRun.ID,
	).Scan(&runState))
	require.Equal(t, runStateFailed, runState)
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM torrents WHERE info_hash=$1`, hashG,
	).Scan(&remaining))
	require.Equal(t, 1, remaining)
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM junkpurge_judgments WHERE info_hash=$1`,
		hashG,
	).Scan(&judgments))
	require.Zero(t, judgments)

	// Provider usage is a lifetime item ledger, not just the final attempt.
	// Tokens reported by a retryable attempt remain billed when a later
	// attempt succeeds.
	hashI := bytes20(9)
	insertCandidate(t, pool, hashI, "retry usage")
	workerCfg.LLMBatchMaxAttempts = 2
	retryRun, err := createBatchRun(ctx, pool, workerCfg)
	require.NoError(t, err)
	require.NotNil(t, retryRun)
	retryAttemptOne, err := prepareBatchAttempt(
		ctx, pool, *retryRun, client.BuildInput,
	)
	require.NoError(t, err)
	retryItems, err := loadAttemptItems(ctx, pool, retryAttemptOne.ID)
	require.NoError(t, err)
	require.Len(t, retryItems, 1)
	claimed, err = claimBatchAttempt(
		ctx, pool, retryAttemptOne.ID, "ingest-retry-1", time.Minute,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	retrySummary, err := ingestBatchAttempt(
		ctx, pool, *retryAttemptOne, "expired",
		[]batchItemResult{{
			CustomID:       retryItems[0].CustomID,
			Retryable:      true,
			ResponseStatus: 429,
			ErrorCode:      "rate_limit_exceeded",
			Usage:          tokenUsage{Input: 5, Output: 1},
		}},
		"ingest-retry-1",
	)
	require.NoError(t, err)
	require.Equal(t, runStateActive, retrySummary.RunState)
	require.NoError(t, releaseBatchAttempt(
		ctx, pool, retryAttemptOne.ID, "ingest-retry-1",
	))

	retryAttemptTwo, err := prepareBatchAttempt(
		ctx, pool, *retryRun, client.BuildInput,
	)
	require.NoError(t, err)
	require.NotNil(t, retryAttemptTwo)
	claimed, err = claimBatchAttempt(
		ctx, pool, retryAttemptTwo.ID, "ingest-retry-2", time.Minute,
	)
	require.NoError(t, err)
	require.True(t, claimed)
	retrySummary, err = ingestBatchAttempt(
		ctx, pool, *retryAttemptTwo, "completed",
		[]batchItemResult{{
			CustomID:  retryItems[0].CustomID,
			Succeeded: true,
			Judgment: Judgment{
				Verdict: verdictRealMangled, Confidence: 0.95,
			},
			ResponseStatus: 200,
			Usage:          tokenUsage{Input: 7, Output: 2},
		}},
		"ingest-retry-2",
	)
	require.NoError(t, err)
	require.Equal(t, runStateFinalizing, retrySummary.RunState)
	require.NoError(t, releaseBatchAttempt(
		ctx, pool, retryAttemptTwo.ID, "ingest-retry-2",
	))
	var lifetimeInput, lifetimeOutput int64
	require.NoError(t, pool.QueryRow(ctx, `
SELECT input_tokens, output_tokens
FROM junkpurge_batch_items
WHERE run_id=$1 AND info_hash=$2`,
		retryRun.ID, hashI,
	).Scan(&lifetimeInput, &lifetimeOutput))
	require.Equal(t, int64(12), lifetimeInput)
	require.Equal(t, int64(3), lifetimeOutput)
	retryFinalized, err := finalizeBatchRun(ctx, pool, *retryRun, false)
	require.NoError(t, err)
	require.Equal(t, runStateDryRun, retryFinalized.State)

	// Cross-mode admission is serialized before either mode takes its
	// candidate snapshot. Batch must wait behind a synchronous reservation
	// transaction and then observe its newly committed claim.
	hashJ := bytes20(10)
	insertCandidate(t, pool, hashJ, "cross mode race")
	syncTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, func() error {
		if _, execErr := syncTx.Exec(ctx, batchAdmissionLockSQL); execErr != nil {
			return execErr
		}
		var name string
		if queryErr := syncTx.QueryRow(ctx, `
SELECT name FROM torrents WHERE info_hash=$1 FOR UPDATE`, hashJ).Scan(&name); queryErr != nil {
			return queryErr
		}
		if name != "cross mode race" {
			return fmt.Errorf("unexpected locked name %q", name)
		}
		_, execErr := syncTx.Exec(ctx, `
INSERT INTO junkpurge_sync_claims (
  info_hash, owner, torrent_name, claimed_at, lease_until
) VALUES ($1,'sync-race','cross mode race',now(),now()+interval '1 hour')`,
			hashJ,
		)
		return execErr
	}())

	type runResult struct {
		run *batchRun
		err error
	}
	runResultCh := make(chan runResult, 1)
	workerCfg.LLMBatchMaxInFlight = 3
	go func() {
		raceCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		created, createErr := createBatchRun(raceCtx, pool, workerCfg)
		runResultCh <- runResult{run: created, err: createErr}
	}()
	select {
	case premature := <-runResultCh:
		_ = syncTx.Rollback(ctx)
		t.Fatalf("Batch reservation did not wait for sync admission: %+v", premature)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, syncTx.Commit(ctx))
	select {
	case result := <-runResultCh:
		require.NoError(t, result.err)
		require.Nil(t, result.run)
	case <-time.After(5 * time.Second):
		t.Fatal("Batch reservation did not resume after sync admission commit")
	}

	// Standard-mode claims serialize replicas and settlement rechecks current
	// eligibility before deletion.
	hashE := bytes20(5)
	insertCandidate(t, pool, hashE, "sync junk")
	owned, err := claimSyncCandidate(
		ctx, pool, hashE, "sync junk", "sync-a", time.Hour, workerCfg,
	)
	require.NoError(t, err)
	require.True(t, owned)
	owned, err = claimSyncCandidate(
		ctx, pool, hashE, "sync junk", "sync-b", time.Hour, workerCfg,
	)
	require.NoError(t, err)
	require.False(t, owned)
	require.NoError(t, recordJudgment(
		ctx, pool, hashE,
		Judgment{Verdict: verdictJunk, Confidence: 0.99}, "sync junk",
	))
	quarantinedHashes, err := quarantineJunk(
		ctx, pool, [][]byte{hashE}, 0.8, 7*24*time.Hour, "sync-a",
	)
	require.NoError(t, err)
	require.Equal(t, [][]byte{hashE}, quarantinedHashes)
	require.NoError(t, releaseSyncClaims(
		ctx, pool, "sync-a", [][]byte{hashE},
	))

	hashF := bytes20(6)
	insertCandidate(t, pool, hashF, "became matched")
	owned, err = claimSyncCandidate(
		ctx, pool, hashF, "became matched", "sync-a", time.Hour, workerCfg,
	)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, recordJudgment(
		ctx, pool, hashF,
		Judgment{Verdict: verdictJunk, Confidence: 0.99}, "became matched",
	))
	_, err = pool.Exec(ctx,
		`UPDATE torrent_contents SET content_id='tmdb-123' WHERE info_hash=$1`,
		hashF,
	)
	require.NoError(t, err)
	quarantinedHashes, err = quarantineJunk(
		ctx, pool, [][]byte{hashF}, 0.8, 7*24*time.Hour, "sync-a",
	)
	require.NoError(t, err)
	require.Empty(t, quarantinedHashes)
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM torrents WHERE info_hash=$1`, hashF,
	).Scan(&remaining))
	require.Equal(t, 1, remaining)
	require.NoError(t, releaseSyncClaims(
		ctx, pool, "sync-a", [][]byte{hashF},
	))

	hashL := bytes20(12)
	insertCandidate(t, pool, hashL, "matched during llm")
	owned, err = claimSyncCandidate(
		ctx, pool, hashL, "matched during llm", "sync-stale", time.Hour, workerCfg,
	)
	require.NoError(t, err)
	require.True(t, owned)
	_, err = pool.Exec(ctx,
		`UPDATE torrent_contents SET content_id='tmdb-456' WHERE info_hash=$1`,
		hashL,
	)
	require.NoError(t, err)
	recorded, err := recordClaimedJudgment(
		ctx, pool, hashL,
		Judgment{Verdict: verdictJunk, Confidence: 0.99},
		"matched during llm", "sync-stale", workerCfg,
	)
	require.NoError(t, err)
	require.False(t, recorded)
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM junkpurge_judgments WHERE info_hash=$1`,
		hashL,
	).Scan(&judgments))
	require.Zero(t, judgments)
	require.NoError(t, settleSyncClaims(
		ctx, pool, "sync-stale", [][]byte{hashL}, nil, nil,
		workerCfg.RejudgeInterval, workerCfg.LLMFailureCooldown,
	))

	// An all-error cycle records no judgments. A nil judged slice must release
	// every claim immediately instead of encoding to SQL NULL and leaking the
	// long admission lease.
	hashH := bytes20(8)
	insertCandidate(t, pool, hashH, "sync judge failed")
	owned, err = claimSyncCandidate(
		ctx, pool, hashH, "sync judge failed", "sync-errors", time.Hour, workerCfg,
	)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, settleSyncClaims(
		ctx, pool, "sync-errors", [][]byte{hashH}, nil, nil,
		workerCfg.RejudgeInterval, workerCfg.LLMFailureCooldown,
	))
	var claims int
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM junkpurge_sync_claims WHERE info_hash=$1`,
		hashH,
	).Scan(&claims))
	require.Zero(t, claims)

	// a judge-REJECTED reply must cool the item down, not release it.
	// Releasing is what let one torrent whose reply carried a one-character
	// typo in the verdict enum be re-selected and re-billed on every cycle for
	// ~967 cycles. The claim has to survive with a future lease, because the
	// candidate query's own `lease_until > now()` exclusion is what keeps the
	// item out of the next cycle.
	hashCool := bytes20(41)
	insertCandidate(t, pool, hashCool, "sync judge rejected the reply")
	owned, err = claimSyncCandidate(
		ctx, pool, hashCool, "sync judge rejected the reply", "sync-cooldown",
		time.Hour, workerCfg,
	)
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, settleSyncClaims(
		ctx, pool, "sync-cooldown", [][]byte{hashCool}, nil,
		[][]byte{hashCool}, workerCfg.RejudgeInterval, 24*time.Hour,
	))

	var cooledLease time.Time
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT lease_until FROM junkpurge_sync_claims WHERE info_hash=$1`,
		hashCool,
	).Scan(&cooledLease), "cooled claim must survive settle, not be deleted")
	require.True(t, cooledLease.After(time.Now().Add(23*time.Hour)),
		"cooled claim lease should sit ~24h out, got %s", cooledLease)

	// The behaviour that actually matters: the candidate query skips it.
	//
	// BatchSize matters here and is easy to get wrong: by this point in the
	// test workerCfg.BatchSize is 1, and candidateQuery is
	// `ORDER BY t.created_at ASC LIMIT $3`. hashCool is inserted last, so it
	// sorts last and falls outside a LIMIT 1 whether it is cooled or not —
	// which would make BOTH assertions below pass for the wrong reason.
	eligCfg := workerCfg
	eligCfg.BatchSize = 500
	afterCooldown, err := findCandidates(ctx, pool, eligCfg)
	require.NoError(t, err)
	for _, c := range afterCooldown {
		require.NotEqual(t, hashCool, c.infoHash,
			"a cooled item must not be re-selected on the next cycle")
	}

	// And it comes BACK once the cooldown expires — this is a retry delay, not
	// a permanent drop. A fix that silently retired the item would also pass
	// the assertion above, so prove the other direction too.
	// claimed_at moves back too: the table carries check(lease_until >
	// claimed_at), so expiry cannot be simulated by lowering the lease alone.
	// That constraint is also why the cooldown UPDATE is safe by construction
	// — greatest() can only ever extend a lease, never pull it below the claim.
	_, err = pool.Exec(ctx,
		`UPDATE junkpurge_sync_claims
		 SET claimed_at=now()-interval '2 hours',
		     lease_until=now()-interval '1 minute'
		 WHERE info_hash=$1`, hashCool)
	require.NoError(t, err)
	afterExpiry, err := findCandidates(ctx, pool, eligCfg)
	require.NoError(t, err)
	var found bool
	for _, c := range afterExpiry {
		if bytes.Equal(c.infoHash, hashCool) {
			found = true
		}
	}
	require.True(t, found, "an expired cooldown must make the item eligible again")

	// The store-level assertions above prove settleSyncClaims honours a cooled
	// set. They do NOT prove the worker ever puts anything in it — verified by
	// mutation: reverting the worker's `cooledClaims = append(...)` left every
	// assertion above green. That gap is the actual bug, so drive a real
	// cycle with a judge that always rejects and assert the claim survives.
	hashLoop := bytes20(42)
	insertCandidate(t, pool, hashLoop, "worker cycle judge always rejects")
	loopCfg := workerCfg
	loopCfg.Enabled = true
	loopCfg.EnablePurge = false // observation-only; this test is about claims
	loopCfg.BatchSize = 500
	loopCfg.LLMFailureCooldown = 24 * time.Hour
	w := &purgeWorker{
		cfg:             loopCfg,
		pool:            lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }),
		judge:           rejectingJudge{},
		metrics:         NewMetrics(),
		logger:          zap.NewNop().Sugar(),
		batchLeaseOwner: "sync-worker-loop",
	}
	w.runCycle(ctx)

	var loopLease time.Time
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT lease_until FROM junkpurge_sync_claims WHERE info_hash=$1`,
		hashLoop,
	).Scan(&loopLease),
		"a judge-rejected item must keep its claim so it is not re-billed next cycle")
	require.True(t, loopLease.After(time.Now().Add(23*time.Hour)),
		"worker should cool a rejected item for ~24h, lease=%s", loopLease)

	var loopJudgments int
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT count(*) FROM junkpurge_judgments WHERE info_hash=$1`,
		hashLoop,
	).Scan(&loopJudgments))
	require.Zero(t, loopJudgments, "a rejected reply must never become a verdict")

	// A positive-but-sub-second cooldown passes validation, so it must also be
	// honoured. int64(d/time.Second) floors it to 0 and make_interval then
	// extends the lease by nothing, silently restoring immediate retry.
	//
	// greatest() hides that unless the row's existing lease is already past —
	// with an hour-long claim still live, a zero extension looks identical to
	// a correct one. So expire the claim first, then settle.
	hashSub := bytes20(43)
	insertCandidate(t, pool, hashSub, "sub-second cooldown must not floor to zero")
	owned, err = claimSyncCandidate(
		ctx, pool, hashSub, "sub-second cooldown must not floor to zero",
		"sync-subsecond", time.Hour, workerCfg,
	)
	require.NoError(t, err)
	require.True(t, owned)
	_, err = pool.Exec(ctx,
		`UPDATE junkpurge_sync_claims
		 SET claimed_at=now()-interval '2 hours',
		     lease_until=now()-interval '1 minute'
		 WHERE info_hash=$1`, hashSub)
	require.NoError(t, err)

	settledAt := time.Now()
	require.NoError(t, settleSyncClaims(
		ctx, pool, "sync-subsecond", [][]byte{hashSub}, nil,
		[][]byte{hashSub}, workerCfg.RejudgeInterval, 900*time.Millisecond,
	))
	var subLease time.Time
	require.NoError(t, pool.QueryRow(
		ctx, `SELECT lease_until FROM junkpurge_sync_claims WHERE info_hash=$1`,
		hashSub,
	).Scan(&subLease))
	// Floored to 0 the lease lands on ~now; honoured it lands ~900ms out. The
	// 500ms threshold separates the two without racing the round-trip.
	require.True(t, subLease.After(settledAt.Add(500*time.Millisecond)),
		"a 900ms cooldown must extend the lease by ~900ms, not floor to 0; lease=%s settled=%s",
		subLease, settledAt)
}

// rejectingJudge reproduces the production failure: a well-formed reply whose
// verdict enum is misspelled, which parseJudgment correctly refuses. Not
// ErrLLMUnavailable — the endpoint is up, this one answer is unusable.
type rejectingJudge struct{}

func (rejectingJudge) Judge(context.Context, string) (Judgment, error) {
	return Judgment{}, fmt.Errorf(
		"junkpurge llm: verdict: unsupported verdict %q", "real_manged",
	)
}

func (j rejectingJudge) JudgeBatch(ctx context.Context, names []string) ([]Judgment, error) {
	return nil, fmt.Errorf(
		"junkpurge llm: grouped verdict: unsupported verdict %q", "real_manged",
	)
}

func bytes20(value byte) []byte {
	return []byte{
		value, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, value,
	}
}

func insertCandidate(
	t *testing.T,
	pool *pgxpool.Pool,
	hash []byte,
	name string,
) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO torrents (info_hash,name,created_at,updated_at)
VALUES ($1,$2,now()-interval '30 days',now());
INSERT INTO torrent_contents (info_hash,content_type,content_id,created_at)
VALUES ($1,'movie',NULL,now()-interval '30 days')`,
		hash, name,
	)
	require.NoError(t, err)
}

type privacyBoundaryBatchClient struct {
	baseURL        string
	findInputCalls int
	uploadCalls    int
	findBatchCalls int
	createCalls    int
	deletedFiles   []string
	failDeletes    bool
}

func (c *privacyBoundaryBatchClient) BaseURL() string {
	return c.baseURL
}

func (c *privacyBoundaryBatchClient) BuildInput(
	batchRun,
	int,
	[]batchItem,
) ([]byte, string, string, error) {
	return nil, "", "", fmt.Errorf("unexpected BuildInput")
}

func (c *privacyBoundaryBatchClient) FindInputFile(
	context.Context,
	string,
	string,
	int64,
) (batchWorkerFile, bool, error) {
	c.findInputCalls++
	return batchWorkerFile{}, false, nil
}

func (c *privacyBoundaryBatchClient) UploadInputFile(
	context.Context,
	string,
	[]byte,
) (batchWorkerFile, error) {
	c.uploadCalls++
	return batchWorkerFile{}, fmt.Errorf("unsafe legacy input was uploaded")
}

func (c *privacyBoundaryBatchClient) FindBatch(
	context.Context,
	string,
	string,
	map[string]string,
) (batchWorkerJob, bool, error) {
	c.findBatchCalls++
	return batchWorkerJob{}, false, nil
}

func (c *privacyBoundaryBatchClient) CreateBatch(
	context.Context,
	batchWorkerCreate,
) (batchWorkerJob, error) {
	c.createCalls++
	return batchWorkerJob{}, fmt.Errorf("unsafe legacy Batch was created")
}

func (c *privacyBoundaryBatchClient) RetrieveBatch(
	context.Context,
	string,
) (batchWorkerJob, error) {
	return batchWorkerJob{}, fmt.Errorf("unexpected RetrieveBatch")
}

func (c *privacyBoundaryBatchClient) DownloadFile(
	context.Context,
	string,
) ([]byte, error) {
	return nil, fmt.Errorf("unexpected DownloadFile")
}

func (c *privacyBoundaryBatchClient) ParseResults(
	[]byte,
	[]byte,
) ([]batchItemResult, error) {
	return nil, fmt.Errorf("unexpected ParseResults")
}

func (c *privacyBoundaryBatchClient) DeleteFile(
	_ context.Context,
	id string,
) error {
	c.deletedFiles = append(c.deletedFiles, id)
	if c.failDeletes {
		return fmt.Errorf("transient provider delete failure")
	}

	return nil
}

func attemptIDs(attempts []batchAttempt) []int64 {
	ids := make([]int64, 0, len(attempts))
	for _, attempt := range attempts {
		ids = append(ids, attempt.ID)
	}

	return ids
}
