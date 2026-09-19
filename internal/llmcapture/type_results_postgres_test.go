package llmcapture

import (
	"bytes"
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
	"github.com/spencercnorton/bitagent/internal/lazy"
	"github.com/stretchr/testify/require"
)

func typeResultPostgresFixture(t *testing.T) (*Recorder, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("bitagent_type_result_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		defer admin.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, cleanupErr := admin.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE")
		require.NoError(t, cleanupErr)
	})
	pcfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `CREATE TABLE torrents(info_hash bytea PRIMARY KEY, private boolean NOT NULL);
CREATE TABLE label_evidence(info_hash bytea, source text, category text);`)
	require.NoError(t, err)
	for _, path := range []string{
		"../../migrations/00046_llm_evaluation_capture.sql",
		"../../migrations/00051_llm_capture_results.sql",
		"../../migrations/00052_classifier_type_capture.sql",
		"../../migrations/00053_llm_capture_audit_incomplete.sql",
	} {
		raw, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		_, err = pool.Exec(ctx, strings.Split(string(raw), "-- +goose Down")[0])
		require.NoError(t, err)
	}
	store := NewPostgresStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	r, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	return r, pool
}

func TestPostgresClassifierTypeFirstResultDecisionPrivacyAndRetention(t *testing.T) {
	r, pool := typeResultPostgresFixture(t)
	ctx := context.Background()
	req := validRequest()
	req.Task, req.ContractID = TaskClassifierType, "classifier-type-v1"
	req.TaskInputJSON = json.RawMessage(`{"release_name":"A","file_paths":[],"min_confidence":0.75,"live":false}`)
	_, err := pool.Exec(ctx, "INSERT INTO torrents VALUES ($1,false)", req.InfoHash)
	require.NoError(t, err)
	_, err = r.Capture(ctx, req)
	require.NoError(t, err)
	key, err := KeyForRequest(req)
	require.NoError(t, err)
	require.NoError(t, r.RecheckTypeRequest(ctx, key, req.InfoHash))
	require.ErrorIs(t, r.RecheckTypeRequest(ctx, key, bytes.Repeat([]byte{1}, 20)), ErrCaptureUnavailable)
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=true")
	require.NoError(t, err)
	require.ErrorIs(t, r.RecheckTypeRequest(ctx, key, req.InfoHash), ErrCaptureUnavailable, "native privacy can change after capture but before dispatch")
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=false")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO label_evidence VALUES ($1,'qbittorrent','PRIVATE')", req.InfoHash)
	require.NoError(t, err)
	require.ErrorIs(t, r.RecheckTypeRequest(ctx, key, req.InfoHash), ErrCaptureUnavailable, "qB privacy can change after capture but before dispatch")
	_, err = pool.Exec(ctx, "DELETE FROM label_evidence")
	require.NoError(t, err)
	require.NoError(t, r.RecheckTypeRequest(ctx, key, req.InfoHash))
	result := HTTPResult{Body: []byte(`{"choices":[{"message":{"content":"{\"category\":\"movie\",\"confidence\":0.9}"}}]}`), StatusCode: 200, ErrorClass: "none"}
	var wg sync.WaitGroup
	receipts := make(chan ResultReceipt, 12)
	errorsSeen := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, recordErr := r.RecordHTTPResult(ctx, key, result)
			if recordErr != nil {
				errorsSeen <- recordErr
			} else if receipt.FirstObservation {
				receipts <- receipt
			}
		}()
	}
	wg.Wait()
	close(receipts)
	close(errorsSeen)
	for recordErr := range errorsSeen {
		require.NoError(t, recordErr)
	}
	require.Len(t, receipts, 1)
	receipt := <-receipts
	decision := TypeDecision{Outcome: "classified", Category: "movie", Confidence: .9, MinConfidence: .75, WouldApply: true}
	wrongPolicy := decision
	wrongPolicy.MinConfidence = .8
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, wrongPolicy), ErrCaptureUnavailable, "first decision must use captured threshold")
	wrongPolicy = decision
	wrongPolicy.Live = true
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, wrongPolicy), ErrCaptureUnavailable, "first decision must use captured shadow/live mode")
	duplicate := receipt
	duplicate.FirstObservation = false
	require.ErrorIs(t, r.RecordTypeDecision(ctx, duplicate, req.InfoHash, decision), ErrCaptureUnavailable, "duplicate cannot create a missing first decision")
	mismatched, err := r.RecordHTTPResult(ctx, key, HTTPResult{Body: []byte(`{"different":true}`), StatusCode: 200, ErrorClass: "none"})
	require.NoError(t, err)
	require.False(t, mismatched.FirstObservation)
	require.ErrorIs(t, r.RecordTypeDecision(ctx, mismatched, req.InfoHash, decision), ErrCaptureUnavailable)
	bad := receipt
	bad.ResponseSHA256 = bytes.Repeat([]byte{7}, 32)
	require.ErrorIs(t, r.RecordTypeDecision(ctx, bad, req.InfoHash, decision), ErrCaptureUnavailable)
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, bytes.Repeat([]byte{1}, 20), decision), ErrCaptureUnavailable)
	matchDecision := MatchDecision{Outcome: "matched", ChosenID: 1, Confidence: .9, MinConfidence: .75, WouldAttach: true}
	require.ErrorIs(t, r.RecordMatchDecision(ctx, receipt, req.InfoHash, matchDecision), ErrCaptureUnavailable, "type result cannot authorize matcher evidence")
	_, err = pool.Exec(ctx, "INSERT INTO label_evidence VALUES ($1,'qbittorrent','BiTgRaB')", req.InfoHash)
	require.NoError(t, err)
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, decision), ErrCaptureUnavailable)
	_, err = r.RecordHTTPResult(ctx, key, result)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	_, err = pool.Exec(ctx, "DELETE FROM label_evidence")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=true")
	require.NoError(t, err)
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, decision), ErrCaptureUnavailable)
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=false")
	require.NoError(t, err)
	require.NoError(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, decision))
	require.NoError(t, r.RecordTypeDecision(ctx, duplicate, req.InfoHash, decision), "duplicate validates identical existing decision only")
	receipt.FromCache = true
	require.NoError(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, decision), "exact cached first receipt is idempotent")
	changed := decision
	changed.Live = true
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, changed), ErrCaptureUnavailable, "shadow evidence cannot become live evidence")
	changed = decision
	changed.Category = "tv"
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, changed), ErrCaptureUnavailable)
	var retained []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT response_body FROM llm_evaluation_capture_results WHERE capture_key=$1", key).Scan(&retained))
	require.Equal(t, result.Body, retained)
	// Down refuses to remove a task contract while its evidence remains.
	migration, err := os.ReadFile("../../migrations/00052_classifier_type_capture.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, strings.Split(string(migration), "-- +goose Down")[1])
	require.Error(t, err)
	_, err = pool.Exec(ctx, `UPDATE llm_evaluation_captures SET captured_at=now()-interval '2 hours', expires_at=now()-interval '1 hour';
UPDATE llm_evaluation_capture_admissions SET expires_at=now()-interval '1 hour'`)
	require.NoError(t, err)
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, decision), ErrCaptureUnavailable)
	require.ErrorIs(t, r.RecheckTypeRequest(ctx, key, req.InfoHash), ErrCaptureUnavailable)
	removed, err := r.store.(*PostgresStore).DeleteExpired(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM llm_evaluation_capture_results").Scan(&count))
	require.Zero(t, count)
	_, err = pool.Exec(ctx, strings.Split(string(migration), "-- +goose Down")[1])
	require.NoError(t, err, "empty type history permits non-destructive rollback")
}

func TestPostgresTypeDecisionRejectsOtherTasksAndFailedHTTP(t *testing.T) {
	r, pool := typeResultPostgresFixture(t)
	ctx := context.Background()
	decision := TypeDecision{Outcome: "classified", Category: "movie", Confidence: .9, MinConfidence: .75, WouldApply: true, Live: true}
	for i, tc := range []struct {
		task   Task
		source CandidateSource
		status int
		class  string
	}{
		{TaskMatcherRerank, CandidateSourceLocal, 200, "none"},
		{TaskMatcherExtract, CandidateSourceNone, 200, "none"},
		{TaskClassifierType, CandidateSourceNone, 503, "http_status"},
		{TaskClassifierType, CandidateSourceNone, 200, "envelope"},
	} {
		req := validRequest()
		req.InfoHash[0] = byte(i + 1)
		req.Task, req.CandidateSource = tc.task, tc.source
		req.TaskInputJSON = json.RawMessage(`{"release_name":"A","file_paths":[],"min_confidence":0.75,"live":true}`)
		_, err := pool.Exec(ctx, "INSERT INTO torrents VALUES ($1,false)", req.InfoHash)
		require.NoError(t, err)
		_, err = r.Capture(ctx, req)
		require.NoError(t, err)
		key, err := KeyForRequest(req)
		require.NoError(t, err)
		if tc.task != TaskClassifierType {
			require.ErrorIs(t, r.RecheckTypeRequest(ctx, key, req.InfoHash), ErrCaptureUnavailable, "other tasks cannot confer type dispatch authority")
		}
		receipt, err := r.RecordHTTPResult(ctx, key, HTTPResult{Body: []byte(`{}`), StatusCode: tc.status, ErrorClass: tc.class})
		require.NoError(t, err)
		// The database verifies stored status/task even if a caller tampers with
		// the receipt's convenience fields.
		receipt.StatusCode, receipt.ErrorClass = 200, "none"
		require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, decision), ErrCaptureUnavailable)
	}
}

func TestPostgresTypeDecisionRejectsMissingOrMalformedCapturedPolicy(t *testing.T) {
	r, pool := typeResultPostgresFixture(t)
	ctx := context.Background()
	decision := TypeDecision{Outcome: "classified", Category: "movie", Confidence: .9, MinConfidence: .75, WouldApply: true}
	for i, input := range []string{
		`{}`,
		`{"min_confidence":"0.75","live":false}`,
		`{"min_confidence":0.75,"live":"false"}`,
		`{"min_confidence":0.75}`,
		`{"live":false}`,
	} {
		req := validRequest()
		req.InfoHash[0] = byte(i + 1)
		req.Task = TaskClassifierType
		req.TaskInputJSON = json.RawMessage(input)
		_, err := pool.Exec(ctx, "INSERT INTO torrents VALUES ($1,false)", req.InfoHash)
		require.NoError(t, err)
		_, err = r.Capture(ctx, req)
		require.NoError(t, err)
		key, err := KeyForRequest(req)
		require.NoError(t, err)
		receipt, err := r.RecordHTTPResult(ctx, key, HTTPResult{Body: []byte(`{}`), StatusCode: 200, ErrorClass: "none"})
		require.NoError(t, err)
		require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, decision), ErrCaptureUnavailable)
	}
}
