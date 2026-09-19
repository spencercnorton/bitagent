package llmcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentFilterDecisionRecorderValidatesPolicy(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	store := &resultStoreStub{first: true}
	r, err := NewRecorder(cfg, &privacyStub{}, store)
	require.NoError(t, err)
	receipt := ResultReceipt{
		CaptureKey: make([]byte, 32), ResponseSHA256: make([]byte, 32),
		FirstObservation: true, StatusCode: 200, ErrorClass: "none",
	}
	decision := ContentFilterDecision{
		Outcome: "non_english", Confidence: .92, Reason: "spanish-article",
		MinConfidence: .85, WouldDrop: true,
	}
	require.NoError(t, r.RecordContentFilterDecision(context.Background(), receipt, make([]byte, 20), decision))
	var retained ContentFilterDecision
	require.NoError(t, json.Unmarshal(store.decisions[0], &retained))
	require.Equal(t, decision, retained)

	for _, bad := range []ContentFilterDecision{
		{Outcome: "non_english", Confidence: .8, Reason: "x", MinConfidence: .85, WouldDrop: true},
		{Outcome: "english", IsEnglish: true, Confidence: .9, MinConfidence: .85, WouldDrop: true, Reason: "english-clear"},
		{Outcome: "low_confidence", Confidence: .5, MinConfidence: .85, WouldDrop: true, Reason: "ambiguous"},
		{Outcome: "invalid_response", Confidence: math.NaN(), MinConfidence: .85},
	} {
		require.ErrorIs(t, r.RecordContentFilterDecision(context.Background(), receipt, make([]byte, 20), bad), ErrCaptureUnavailable)
	}
	errorReceipt := receipt
	errorReceipt.StatusCode, errorReceipt.ErrorClass = 429, "http_status"
	require.ErrorIs(t, r.RecordContentFilterDecision(context.Background(), errorReceipt, make([]byte, 20), decision), ErrCaptureUnavailable,
		"provider errors cannot authorize an actionable non-English decision")
	require.NoError(t, r.RecordContentFilterDecision(context.Background(), errorReceipt, make([]byte, 20), ContentFilterDecision{
		Outcome: "invalid_response", MinConfidence: .85,
	}))
	incompleteReceipt := receipt
	incompleteReceipt.StatusCode, incompleteReceipt.ErrorClass = 0, "audit_incomplete"
	require.NoError(t, r.RecordContentFilterDecision(context.Background(), incompleteReceipt, make([]byte, 20), ContentFilterDecision{
		Outcome: "audit_incomplete", MinConfidence: .85,
	}))
}

func TestValidContentFilterDecisionOutcomes(t *testing.T) {
	for _, decision := range []ContentFilterDecision{
		{Outcome: "english", IsEnglish: true, Confidence: .9, Reason: "english-clear", MinConfidence: .85},
		{Outcome: "non_english", Confidence: .9, Reason: "spanish-article", MinConfidence: .85, WouldDrop: true},
		{Outcome: "low_confidence", Confidence: .5, Reason: "ambiguous", MinConfidence: .85},
		{Outcome: "invalid_response", MinConfidence: .85},
		{Outcome: "audit_incomplete", MinConfidence: .85},
	} {
		require.True(t, validContentFilterDecision(decision), "%+v", decision)
	}
}

func TestPostgresContentFilterResultDecisionPrivacyAndPolicyBinding(t *testing.T) {
	r, pool := typeResultPostgresFixture(t)
	ctx := context.Background()
	req := validRequest()
	req.Task, req.ContractID = TaskContentFilter, "contentfilter-chat-model-input-v2-openrouter"
	req.TaskInputJSON = json.RawMessage(`{"title":"Pelicula","min_confidence":0.85,"live":false,"eligibility_input":{}}`)
	_, err := pool.Exec(ctx, "INSERT INTO torrents VALUES ($1,false)", req.InfoHash)
	require.NoError(t, err)
	_, err = r.Capture(ctx, req)
	require.NoError(t, err)
	key, err := KeyForRequest(req)
	require.NoError(t, err)
	replay, err := r.FindContentFilterReplay(ctx, key, req.InfoHash)
	require.NoError(t, err)
	require.True(t, replay.Found)
	require.Nil(t, replay.Decision)
	require.NoError(t, r.RecheckContentFilterRequest(ctx, key, req.InfoHash))
	require.ErrorIs(t, r.RecheckContentFilterRequest(ctx, key, bytes.Repeat([]byte{7}, 20)), ErrCaptureUnavailable)

	result := HTTPResult{Body: []byte(`{"choices":[{"message":{"content":"{\"is_english\":false,\"confidence\":0.92,\"reason\":\"spanish-article\"}"}}]}`), StatusCode: 200, ErrorClass: "none"}
	receipt, err := r.RecordHTTPResult(ctx, key, result)
	require.NoError(t, err)
	replay, err = r.FindContentFilterReplay(ctx, key, req.InfoHash)
	require.NoError(t, err)
	require.NotNil(t, replay.Result)
	require.Nil(t, replay.Decision)
	require.Equal(t, receipt.ResponseSHA256, replay.Result.ResponseSHA256)
	decision := ContentFilterDecision{Outcome: "non_english", Confidence: .92, Reason: "spanish-article", MinConfidence: .85, WouldDrop: true}
	wrong := decision
	wrong.MinConfidence = .9
	require.ErrorIs(t, r.RecordContentFilterDecision(ctx, receipt, req.InfoHash, wrong), ErrCaptureUnavailable)
	wrong = decision
	wrong.Live = true
	require.ErrorIs(t, r.RecordContentFilterDecision(ctx, receipt, req.InfoHash, wrong), ErrCaptureUnavailable)
	require.NoError(t, r.RecordContentFilterDecision(ctx, receipt, req.InfoHash, decision))
	replay, err = r.FindContentFilterReplay(ctx, key, req.InfoHash)
	require.NoError(t, err)
	require.True(t, replay.Found)
	require.Equal(t, decision, *replay.Decision)

	req2 := req
	req2.InfoHash = bytes.Repeat([]byte{9}, 20)
	req2.GroupKey = []byte("rate-limited")
	req2.ModelInputJSON = json.RawMessage(`{"model":"test","input":"rate-limited"}`)
	req2.TaskInputJSON = json.RawMessage(`{"title":"Rate Limited","min_confidence":0.85,"live":false,"eligibility_input":{}}`)
	_, err = pool.Exec(ctx, "INSERT INTO torrents VALUES ($1,false)", req2.InfoHash)
	require.NoError(t, err)
	_, err = r.Capture(ctx, req2)
	require.NoError(t, err)
	key2, err := KeyForRequest(req2)
	require.NoError(t, err)
	errorReceipt, err := r.RecordHTTPResult(ctx, key2, HTTPResult{
		Body: []byte(`{"error":"rate limited"}`), StatusCode: 429, ErrorClass: "http_status",
	})
	require.NoError(t, err)
	invalid := ContentFilterDecision{Outcome: "invalid_response", MinConfidence: .85}
	require.NoError(t, r.RecordContentFilterDecision(ctx, errorReceipt, req2.InfoHash, invalid))
	replay, err = r.FindContentFilterReplay(ctx, key2, req2.InfoHash)
	require.NoError(t, err)
	require.True(t, replay.Found)
	require.Equal(t, invalid, *replay.Decision)

	_, err = pool.Exec(ctx, "UPDATE torrents SET private=true WHERE info_hash=$1", req.InfoHash)
	require.NoError(t, err)
	require.ErrorIs(t, r.RecheckContentFilterRequest(ctx, key, req.InfoHash), ErrCaptureUnavailable)
	_, err = r.FindContentFilterReplay(ctx, key, req.InfoHash)
	require.ErrorIs(t, err, ErrCaptureUnavailable)
	require.ErrorIs(t, r.RecordContentFilterDecision(ctx, receipt, req.InfoHash, decision), ErrCaptureUnavailable)
}
