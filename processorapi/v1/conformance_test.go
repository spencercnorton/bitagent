package v1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublishedFixtures(t *testing.T) {
	read := func(name string, out any) {
		t.Helper()
		b, e := os.ReadFile(filepath.Join("..", "..", "docs", "processors", "fixtures", name))
		require.NoError(t, e)
		require.NoError(t, Decode(b, out))
	}
	var m Manifest
	read("manifest.json", &m)
	require.NoError(t, m.Validate())
	var j Job
	read("job.json", &j)
	require.NoError(t, j.Validate())
	var r Result
	read("result.json", &r)
	require.NoError(t, r.Validate(j))
	var s ServiceStatus
	read("service-status.json", &s)
	require.NoError(t, s.Validate())
	var status Status
	read("operator-status.json", &status)
	require.Equal(t, StatusContract, status.Contract)
	var page JobPage
	read("operator-jobs.json", &page)
	require.NotEmpty(t, page.Items)
	var admission AdmissionPolicy
	read("admission.json", &admission)
	require.Equal(t, AdmissionContract, admission.Contract)
	require.Equal(t, AdmissionAlgorithm, admission.Algorithm)
	require.Equal(t, 100, admission.BasisPoints)
	for _, outcome := range []Outcome{Abstained, Deferred, Failed} {
		copy := r
		copy.Outcome = outcome
		copy.Proposal = nil
		require.NoError(t, copy.Validate(j))
	}
	copy := r
	copy.Outcome = Unmatched
	copy.Proposal = nil
	require.ErrorContains(t, copy.Validate(j), "unproven_complete_miss")
	// A normal processing receipt does not fabricate a matching receipt.
	j.Stage = Match
	r.Binding = j.Binding
	r.Outcome = Unmatched
	r.Proposal = nil
	require.Error(t, r.Validate(j))
	j.Input.Deterministic.MatchingOutcome = "completed_unmatched"
	require.NoError(t, r.Validate(j))
	r.Processor.ModelVersion = "changed"
	require.ErrorContains(t, r.Validate(j), "binding_mismatch")
}
func TestStageProposalRules(t *testing.T) {
	raw, e := os.ReadFile(filepath.Join("..", "..", "docs", "processors", "fixtures", "job.json"))
	require.NoError(t, e)
	var j Job
	require.NoError(t, json.Unmarshal(raw, &j))
	for _, stage := range Stages {
		t.Run(string(stage), func(t *testing.T) {
			job := j
			job.Stage = stage
			r := Result{Binding: job.Binding, Outcome: Completed, Proposal: &Proposal{ContentType: "movie"}}
			if stage == Classify {
				require.NoError(t, r.Validate(job))
			} else {
				require.Error(t, r.Validate(job))
			}
		})
	}
	j.Stage = Policy
	r := Result{Binding: j.Binding, Outcome: Excluded, Proposal: &Proposal{Exclusion: &Exclusion{Scope: "publication", Reason: "synthetic"}}}
	require.NoError(t, r.Validate(j))
	r.Proposal.Exclusion.Scope = "delete"
	require.Error(t, r.Validate(j))
}
func TestDuplicateAndUnknownFields(t *testing.T) {
	for _, raw := range []string{`{"contract":"one","contract":"two"}`, `{"contract":"x","unexpected":1}`, `{} {}`} {
		var s ServiceStatus
		require.Error(t, Decode([]byte(raw), &s))
	}
	var m Manifest
	require.Error(t, Decode([]byte(`{"configuration_schema":{"type":"object","type":"string"}}`), &m))
}
