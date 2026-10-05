package llmcapture

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPostgresEmbeddingCaptureAdmissionFirstResponseAndNoMatchAuthority(t *testing.T) {
	recorder, pool := typeResultPostgresFixture(t)
	ctx := context.Background()
	migration, err := os.ReadFile("../../migrations/00054_matcher_embedding_capture.sql")
	require.NoError(t, err)
	parts := strings.Split(string(migration), "-- +goose Down")
	_, err = pool.Exec(ctx, parts[0])
	require.NoError(t, err)
	request := validRequest()
	request.Task, request.CandidateSource = TaskMatcherEmbedding, CandidateSourceAPI
	_, err = pool.Exec(ctx, "INSERT INTO torrents VALUES ($1,false)", request.InfoHash)
	require.NoError(t, err)
	_, err = recorder.Capture(ctx, request)
	require.NoError(t, err)
	key, err := KeyForRequest(request)
	require.NoError(t, err)
	require.NoError(t, recorder.RecheckEmbeddingRequest(ctx, key, request.InfoHash))
	require.ErrorIs(t, recorder.RecheckEmbeddingRequest(ctx, key, bytes.Repeat([]byte{1}, 20)), ErrCaptureUnavailable)
	result := HTTPResult{Body: []byte(`{"data":[{"index":0,"embedding":[1,0]}]}`), StatusCode: 200, ErrorClass: "none"}
	receipt, err := recorder.RecordHTTPResult(ctx, key, result)
	require.NoError(t, err)
	require.True(t, receipt.FirstObservation)
	duplicate, err := recorder.RecordHTTPResult(ctx, key, HTTPResult{Body: []byte(`{"different":true}`), StatusCode: 200, ErrorClass: "none"})
	require.NoError(t, err)
	require.False(t, duplicate.FirstObservation)
	var retained []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT response_body FROM llm_evaluation_capture_results WHERE capture_key=$1", key).Scan(&retained))
	require.Equal(t, result.Body, retained)
	require.ErrorIs(t, recorder.RecordMatchDecision(ctx, receipt, request.InfoHash, MatchDecision{Outcome: "matched", ChosenID: 1, Confidence: .99, MinConfidence: .75, WouldAttach: true}), ErrCaptureUnavailable, "embedding receipt cannot authorize a matcher attachment")
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=true")
	require.NoError(t, err)
	require.ErrorIs(t, recorder.RecheckEmbeddingRequest(ctx, key, request.InfoHash), ErrCaptureUnavailable)
	_, err = recorder.RecordHTTPResult(ctx, key, result)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	_, err = pool.Exec(ctx, "UPDATE torrents SET private=false")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "INSERT INTO label_evidence VALUES ($1,'qbittorrent','BiTgRaB')", request.InfoHash)
	require.NoError(t, err)
	require.ErrorIs(t, recorder.RecheckEmbeddingRequest(ctx, key, request.InfoHash), ErrCaptureUnavailable)
	_, err = pool.Exec(ctx, "DELETE FROM label_evidence")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, parts[1])
	require.Error(t, err, "rollback must preserve existing embedding evidence")
	_, err = pool.Exec(ctx, "UPDATE llm_evaluation_captures SET expires_at=transaction_timestamp()-interval '1 second', captured_at=transaction_timestamp()-interval '1 day'")
	require.NoError(t, err)
	require.ErrorIs(t, recorder.RecheckEmbeddingRequest(ctx, key, request.InfoHash), ErrCaptureUnavailable)
}
