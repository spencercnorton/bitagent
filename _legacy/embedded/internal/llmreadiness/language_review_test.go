package llmreadiness

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func languageReviewSnapshot(t *testing.T) (Snapshot, string) {
	t.Helper()
	s, _, _ := syntheticSnapshot(t)
	r := &s.Records[0]
	r.Task, r.Contract = "contentfilter", "contentfilter-chat-model-input-v1"
	r.TaskInput = json.RawMessage(`{"title":"Pelicula 2026","min_confidence":0.85,"live":true,"llm_action":"review"}`)
	r.ModelInput = json.RawMessage(`{"model":"fixture-model","messages":[{"role":"user","content":"Pelicula 2026"}]}`)
	body := []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"is_english\":false,\"confidence\":0.93,\"reason\":\"spanish-article\"}"}}]}`)
	response, responseHash := base64.StdEncoding.EncodeToString(body), hash(body)
	r.ResponseBase64, r.ResponseHash = &response, &responseHash
	r.Decision = json.RawMessage(`{"outcome":"non_english","is_english":false,"confidence":0.93,"reason":"spanish-article","min_confidence":0.85,"live":true,"llm_action":"review","would_review":true,"would_drop":false}`)
	m, err := canonicalObject(r.ModelInput)
	require.NoError(t, err)
	p, err := canonicalObject(r.TaskInput)
	require.NoError(t, err)
	r.InputHash = digestParts(m, p)
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	loaded, h, err := LoadSnapshot(raw)
	require.NoError(t, err)
	return loaded, h
}

func TestLanguageReviewErrorsNeverBecomeDestructiveActionEvidence(t *testing.T) {
	s, h := languageReviewSnapshot(t)
	a, b := submissions(t, s, h, "agent_diagnostic", "english", "english")
	report, err := Score(s, h, a, b)
	require.NoError(t, err)
	require.False(t, report.ProductionAuthority)
	for _, m := range report.Tasks {
		require.Equal(t, 1, m.Scored)
		require.Equal(t, 1, m.Reviews)
		require.Equal(t, 1, m.PotentialReviewErrors)
		require.Equal(t, 1, m.ReviewGroups)
		require.Equal(t, 1, m.ReviewErrorGroups)
		require.Equal(t, 1.0, *m.ReviewRiskUpper95)
		require.Zero(t, m.Actions)
		require.Zero(t, m.PotentialHarms)
		require.Zero(t, m.ActionGroups)
		require.Nil(t, m.HarmUpper95, "review cannot supply a deletion-risk bound")
	}
}

func TestLanguageReviewRequiresConsistentCapturedDispositionAndFlags(t *testing.T) {
	s, _ := languageReviewSnapshot(t)
	r := s.Records[0]
	_, _, action, ok := observation(r)
	require.True(t, ok)
	require.False(t, action)
	for _, mutate := range []func(*Record){
		func(r *Record) {
			r.TaskInput = json.RawMessage(`{"title":"Pelicula 2026","min_confidence":0.85,"live":true,"llm_action":"drop"}`)
		},
		func(r *Record) {
			r.TaskInput = json.RawMessage(`{"title":"Pelicula 2026","min_confidence":0.85,"live":true,"llm_action":"tag_and_delete"}`)
		},
		func(r *Record) {
			r.TaskInput = json.RawMessage(`{"title":"Pelicula 2026","min_confidence":0.9,"live":true,"llm_action":"review"}`)
		},
		func(r *Record) {
			r.TaskInput = json.RawMessage(`{"title":"Pelicula 2026","min_confidence":0.85,"live":false,"llm_action":"review"}`)
		},
		func(r *Record) {
			r.Decision = json.RawMessage(`{"outcome":"non_english","is_english":false,"confidence":0.93,"reason":"spanish-article","min_confidence":0.85,"live":true,"llm_action":"review","would_review":true,"would_drop":true}`)
		},
		func(r *Record) {
			r.Decision = json.RawMessage(`{"outcome":"non_english","is_english":false,"confidence":0.93,"reason":"spanish-article","min_confidence":0.85,"live":true,"llm_action":"review","would_drop":false}`)
		},
	} {
		changed := r
		mutate(&changed)
		_, _, _, valid := observation(changed)
		require.False(t, valid, "mismatched capture must be unscorable")
	}
}

func TestLanguageReviewAndLegacyDropPoliciesAreNotPooled(t *testing.T) {
	s, h := languageReviewSnapshot(t)
	r := s.Records[0]
	r.CaptureKey = hash([]byte("legacy-drop"))
	r.TaskInput = json.RawMessage(`{"title":"Pelicula 2026","min_confidence":0.85,"live":true}`)
	r.Decision = json.RawMessage(`{"outcome":"non_english","is_english":false,"confidence":0.93,"reason":"spanish-article","min_confidence":0.85,"live":true,"would_drop":true}`)
	_, _, action, ok := observation(r)
	require.True(t, ok, "pre-disposition captures retain legacy drop semantics")
	require.True(t, action)
	s.Records = append(s.Records, r)
	a, b := submissions(t, s, h, "agent_diagnostic", "non_english", "non_english")
	report, err := Score(s, h, a, b)
	require.NoError(t, err)
	require.Len(t, report.Tasks, 2)
	reviews, actions := 0, 0
	for _, m := range report.Tasks {
		reviews += m.Reviews
		actions += m.Actions
	}
	require.Equal(t, 1, reviews)
	require.Equal(t, 1, actions)
}
