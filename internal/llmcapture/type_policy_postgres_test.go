package llmcapture

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPostgresTypeDecisionBindsExplicitAllowedTypes(t *testing.T) {
	r, pool := typeResultPostgresFixture(t)
	ctx := context.Background()
	req := validRequest()
	req.Task = TaskClassifierType
	req.TaskInputJSON = json.RawMessage(`{"min_confidence":0.75,"live":true,"live_allowed_types":["movie","tv"]}`)
	_, err := pool.Exec(ctx, "INSERT INTO torrents VALUES ($1,false)", req.InfoHash)
	require.NoError(t, err)
	_, err = r.Capture(ctx, req)
	require.NoError(t, err)
	key, err := KeyForRequest(req)
	require.NoError(t, err)
	receipt, err := r.RecordHTTPResult(ctx, key, HTTPResult{Body: []byte(`{}`), StatusCode: 200, ErrorClass: "none"})
	require.NoError(t, err)
	d := TypeDecision{Outcome: "policy_declined", Category: "music", Confidence: .99, MinConfidence: .75, Live: true, LiveAllowedTypes: []string{"movie", "tv"}}
	wrong := d
	wrong.LiveAllowedTypes = []string{"movie"}
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, wrong), ErrCaptureUnavailable, "a different denied policy cannot use the captured receipt")
	wrong.LiveAllowedTypes = []string{"music"}
	wrong.Outcome, wrong.WouldApply = "classified", true
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, wrong), ErrCaptureUnavailable, "a newly permissive policy cannot use a denied receipt")
	wrong = d
	wrong.Outcome, wrong.WouldApply = "classified", true
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, wrong), ErrCaptureUnavailable, "same-policy denied type cannot claim application")
	require.NoError(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, d))
	var retained TypeDecision
	var body []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT decision FROM llm_evaluation_capture_results WHERE capture_key=$1", key).Scan(&body))
	require.NoError(t, json.Unmarshal(body, &retained))
	require.Equal(t, d, retained)
	require.ErrorIs(t, r.RecordTypeDecision(ctx, receipt, req.InfoHash, wrong), ErrCaptureUnavailable, "immutable decision cannot be replaced")
}

func TestTypeDecisionAllowlistValidation(t *testing.T) {
	d := TypeDecision{Outcome: "policy_declined", Category: "music", Confidence: .9, MinConfidence: .75, LiveAllowedTypes: []string{"movie", "tv"}}
	require.True(t, validTypeDecision(d))
	for _, allowed := range [][]string{nil, {}, {"music"}, {"movie", "movie"}, {"tv_show"}} {
		bad := d
		bad.LiveAllowedTypes = allowed
		require.False(t, validTypeDecision(bad))
	}
	bad := d
	bad.Confidence = .7
	require.False(t, validTypeDecision(bad))
	bad = d
	bad.WouldApply = true
	require.False(t, validTypeDecision(bad))
}
