package llmcapture

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSemanticRequestIdentityExcludesOnlyEvaluationBuild(t *testing.T) {
	req := validRequest()
	req.Task, req.CandidateSource = TaskMatcherRerank, CandidateSourceLocal
	req.BuildIdentity = "synthetic-build-a"
	semantic, err := SemanticKeyForRequest(req)
	require.NoError(t, err)
	key, err := KeyForRequest(req)
	require.NoError(t, err)
	next := req
	next.BuildIdentity = "synthetic-build-b"
	again, err := SemanticKeyForRequest(next)
	require.NoError(t, err)
	require.Equal(t, semantic, again)
	capture, err := KeyForRequest(next)
	require.NoError(t, err)
	require.NotEqual(t, key, capture, "evaluation generation remains immutable and build-bound")
	for _, tc := range []struct {
		name   string
		change func(*Request)
	}{
		{"model", func(r *Request) { r.Model += "-new" }},
		{"endpoint", func(r *Request) { r.Endpoint += "/new" }},
		{"prompt_version", func(r *Request) { r.PromptVersion += "-new" }},
		{"prompt", func(r *Request) { r.SystemPrompt += " changed" }},
		{"contract", func(r *Request) { r.ContractID += "-new" }},
		{"model_body", func(r *Request) { r.ModelInputJSON = []byte(`{"changed":"model body"}`) }},
		{"task_policy", func(r *Request) { r.TaskInputJSON = []byte(`{"changed":"task policy"}`) }},
		{"family", func(r *Request) { r.GroupKey = []byte("different family") }},
		{"source", func(r *Request) { r.InfoHash = append([]byte(nil), r.InfoHash...); r.InfoHash[0] ^= 0xff }},
		{"candidate_source", func(r *Request) { r.CandidateSource = CandidateSourceAPI }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := req
			tc.change(&mutated)
			other, err := SemanticKeyForRequest(mutated)
			require.NoError(t, err)
			require.NotEqual(t, semantic, other)
		})
	}
}
