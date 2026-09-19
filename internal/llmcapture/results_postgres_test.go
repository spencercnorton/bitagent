package llmcapture

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
	"github.com/stretchr/testify/require"
)

func TestPostgresCaptureResultFirstObservationPrivacyAndRetention(t *testing.T) {
	dsn := os.Getenv("BITAGENT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BITAGENT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer admin.Close()
	schema := fmt.Sprintf("bitagent_result_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	defer func() { _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); require.NoError(t, err) }()
	pcfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, `CREATE TABLE torrents(info_hash bytea PRIMARY KEY, private boolean NOT NULL);
CREATE TABLE label_evidence(info_hash bytea, source text, category text);`)
	require.NoError(t, err)
	for _, path := range []string{"../../migrations/00046_llm_evaluation_capture.sql", "../../migrations/00051_llm_capture_results.sql"} {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, strings.Split(string(raw), "-- +goose Down")[0])
		require.NoError(t, err)
	}
	store := NewPostgresStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	r, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	request := validRequest()
	request.Task, request.CandidateSource = TaskMatcherRerank, CandidateSourceLocal
	_, err = pool.Exec(ctx, "INSERT INTO torrents VALUES ($1,false)", request.InfoHash)
	require.NoError(t, err)
	_, err = r.Capture(ctx, request)
	require.NoError(t, err)
	key, err := KeyForRequest(request)
	require.NoError(t, err)
	result := HTTPResult{Body: []byte(`{"choices":[{"message":{"content":"{\"tmdb_id\":1,\"confidence\":0.9}"}}]}`), StatusCode: 200, ErrorClass: "none"}
	var wg sync.WaitGroup
	receipts := make(chan ResultReceipt, 12)
	errorsSeen := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := r.RecordHTTPResult(ctx, key, result)
			if err != nil {
				errorsSeen <- err
			} else if receipt.FirstObservation {
				receipts <- receipt
			}
		}()
	}
	wg.Wait()
	close(receipts)
	close(errorsSeen)
	for err := range errorsSeen {
		require.NoError(t, err)
	}
	require.Len(t, receipts, 1, "one immutable response wins concurrent first-observation admission")
	receipt := <-receipts
	other, err := r.RecordHTTPResult(ctx, key, HTTPResult{Body: []byte(`{"different":true}`), StatusCode: 200, ErrorClass: "none"})
	require.NoError(t, err)
	require.False(t, other.FirstObservation)
	var retained []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT response_body FROM llm_evaluation_capture_results WHERE capture_key=$1", key).Scan(&retained))
	require.Equal(t, result.Body, retained)
	decision := MatchDecision{Outcome: "matched", ChosenID: 1, Confidence: .9, MinConfidence: .75, WouldAttach: true}
	duplicate := receipt
	duplicate.FirstObservation = false
	require.ErrorIs(t, r.RecordMatchDecision(ctx, duplicate, request.InfoHash, decision), ErrCaptureUnavailable, "a duplicate response cannot create the missing first decision")
	bad := receipt
	bad.ResponseSHA256 = bytes.Repeat([]byte{7}, 32)
	require.ErrorIs(t, r.RecordMatchDecision(ctx, bad, request.InfoHash, decision), ErrCaptureUnavailable)
	require.ErrorIs(t, r.RecordMatchDecision(ctx, receipt, bytes.Repeat([]byte{1}, 20), decision), ErrCaptureUnavailable)
	_, err = pool.Exec(ctx, "INSERT INTO label_evidence VALUES ($1,'qbittorrent','BiTgRaB')", request.InfoHash)
	require.NoError(t, err)
	require.ErrorIs(t, r.RecordMatchDecision(ctx, receipt, request.InfoHash, decision), ErrCaptureUnavailable)
	_, err = r.RecordHTTPResult(ctx, key, result)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	_, err = pool.Exec(ctx, "DELETE FROM label_evidence")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=true")
	require.NoError(t, err)
	require.ErrorIs(t, r.RecordMatchDecision(ctx, receipt, request.InfoHash, decision), ErrCaptureUnavailable)
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=false")
	require.NoError(t, err)
	require.NoError(t, r.RecordMatchDecision(ctx, receipt, request.InfoHash, decision))
	require.NoError(t, r.RecordMatchDecision(ctx, receipt, request.InfoHash, decision), "first receipt retry is idempotent")
	require.NoError(t, r.RecordMatchDecision(ctx, duplicate, request.InfoHash, decision), "identical duplicate can validate existing evidence only")
	changed := decision
	changed.Confidence = .95
	require.ErrorIs(t, r.RecordMatchDecision(ctx, receipt, request.InfoHash, changed), ErrCaptureUnavailable, "decisions are not replaceable")
	// Retention is inherited from the parent, including its local admission.
	_, err = pool.Exec(ctx, `UPDATE llm_evaluation_captures SET captured_at=now()-interval '2 hours', expires_at=now()-interval '1 hour';
UPDATE llm_evaluation_capture_admissions SET expires_at=now()-interval '1 hour'`)
	require.NoError(t, err)
	_, err = r.RecordHTTPResult(ctx, key, result)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	removed, err := store.DeleteExpired(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM llm_evaluation_capture_results").Scan(&count))
	require.Zero(t, count)
}
