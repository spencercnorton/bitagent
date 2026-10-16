package llmreadiness

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTypePolicyDeclineIsScoredWithoutAnAction(t *testing.T) {
	s, h, _ := syntheticSnapshot(t)
	r := &s.Records[0]
	body := []byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"category\":\"music\",\"confidence\":0.9}"}}]}`)
	response, digest := base64.StdEncoding.EncodeToString(body), hash(body)
	r.ResponseBase64, r.ResponseHash = &response, &digest
	r.TaskInput = json.RawMessage(`{"min_confidence":0.75,"live":true,"live_allowed_types":["movie","tv"]}`)
	r.Decision = json.RawMessage(`{"outcome":"policy_declined","category":"music","confidence":0.9,"min_confidence":0.75,"would_apply":false,"live":true,"live_allowed_types":["movie","tv"]}`)
	prediction, _, action, ok := observation(*r)
	require.True(t, ok)
	require.Equal(t, "music", prediction)
	require.False(t, action)
	a, b := submissions(t, s, h, "operator_reference", "movie", "movie")
	report, err := Score(s, h, a, b)
	require.NoError(t, err)
	require.False(t, report.ProductionAuthority)
	for _, m := range report.Tasks {
		require.Equal(t, 1, m.Scored)
		require.Equal(t, 1, m.PolicyDeclined)
		require.Zero(t, m.Actions)
		require.Zero(t, m.PotentialHarms)
	}
	for _, decision := range []string{
		`{"outcome":"classified","category":"music","confidence":0.9,"min_confidence":0.75,"would_apply":true,"live":true,"live_allowed_types":["movie","tv"]}`,
		`{"outcome":"policy_declined","category":"music","confidence":0.9,"min_confidence":0.75,"would_apply":false,"live":true,"live_allowed_types":["movie"]}`,
		`{"outcome":"policy_declined","category":"music","confidence":0.9,"min_confidence":0.75,"would_apply":false,"live":true,"live_allowed_types":["movie","movie"]}`,
	} {
		r.Decision = json.RawMessage(decision)
		_, _, _, ok := observation(*r)
		require.False(t, ok, "tampered policy/application is not scorable")
	}
}

func TestTypeAllowedPoliciesRemainSeparateScoringBuckets(t *testing.T) {
	s, _, _ := syntheticSnapshot(t)
	r := s.Records[0]
	before := bucketKey(r)
	r.TaskInput = json.RawMessage(`{"min_confidence":0.75,"live":false,"live_allowed_types":["movie","tv"]}`)
	require.NotEqual(t, before, bucketKey(r))
}

func TestTypeNullPolicyIsNotLegacyAbsentPolicy(t *testing.T) {
	s, _, _ := syntheticSnapshot(t)
	r := s.Records[0]
	r.TaskInput = json.RawMessage(`{"min_confidence":0.75,"live":false,"live_allowed_types":null}`)
	_, _, _, ok := observation(r)
	require.False(t, ok)
	r.TaskInput = json.RawMessage(`{"min_confidence":0.75,"live":false}`)
	r.Decision = json.RawMessage(`{"outcome":"classified","category":"movie","confidence":0.9,"min_confidence":0.75,"would_apply":true,"live":false,"live_allowed_types":null}`)
	_, _, _, ok = observation(r)
	require.False(t, ok)
}
