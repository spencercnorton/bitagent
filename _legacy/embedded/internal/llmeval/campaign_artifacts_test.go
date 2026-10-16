package llmeval

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildCampaignAttemptEvidenceIsExactAndDeterministic(t *testing.T) {
	corpus, results, binding := campaignArtifactResults(t)
	reversed := []ResultRecord{results[1], results[0]}

	evidence, err := BuildCampaignAttemptEvidence(reversed)
	require.NoError(t, err)
	require.Equal(t, binding.SystemID, evidence.SystemID)
	require.Equal(t, binding.Task, evidence.Task)
	require.Equal(t, corpus.SHA256, evidence.CorpusSHA256)
	require.Len(t, evidence.Attempts, 2)
	require.Equal(t, results[0].CaseID, evidence.Attempts[0].CaseID)
	require.Equal(t, PromotionAttemptInitial, evidence.Attempts[0].Kind)
	require.Equal(t, 1, evidence.Attempts[0].Sequence)
	require.Equal(t, results[0].Usage, evidence.Attempts[0].Usage)

	identity, err := ResultsIdentity(results)
	require.NoError(t, err)
	require.Equal(t, identity, evidence.ResultsSHA256)
	require.NoError(t, ValidateCampaignAttemptEvidence(results, evidence))

	fromCanonical, err := BuildCampaignAttemptEvidence(results)
	require.NoError(t, err)
	require.Equal(t, fromCanonical, evidence)
}

func TestCampaignAttemptEvidenceRejectsMixedAndTamperedArtifacts(t *testing.T) {
	_, results, binding := campaignArtifactResults(t)
	evidence, err := BuildCampaignAttemptEvidence(results)
	require.NoError(t, err)

	tampered := evidence
	tampered.Attempts = append(
		[]PromotionAttempt(nil),
		evidence.Attempts...,
	)
	tampered.Attempts[0].Usage.CostMicroUSD++
	require.ErrorContains(
		t,
		ValidateCampaignAttemptEvidence(results, tampered),
		"does not exactly match",
	)

	mixed := append([]ResultRecord(nil), results...)
	other := binding
	other.RunID = "run-other-candidate"
	mixed[1].ExecutionAudit.Campaign = &other
	_, err = BuildCampaignAttemptEvidence(mixed)
	require.ErrorContains(t, err, "mixed execution artifact identities")

	unbound := append([]ResultRecord(nil), results...)
	unbound[0].ExecutionAudit.Campaign = nil
	_, err = BuildCampaignAttemptEvidence(unbound)
	require.ErrorContains(t, err, "mixed execution artifact identities")
}

func TestCampaignRunCheckpointRoundTripAndTamperRejection(t *testing.T) {
	_, results, binding := campaignArtifactResults(t)
	checkpoint, err := NewCampaignRunCheckpoint(binding, results)
	require.NoError(t, err)

	var encoded bytes.Buffer
	require.NoError(t, WriteCampaignRunCheckpoint(&encoded, checkpoint))
	decoded, err := ReadCampaignRunCheckpoint(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	require.Equal(t, checkpoint, decoded)

	attemptDigest, err := CampaignAttemptEvidenceIdentity(
		decoded.AttemptEvidence,
	)
	require.NoError(t, err)
	require.Equal(t, checkpoint.AttemptEvidenceSHA256, attemptDigest)

	tampered := checkpoint
	tampered.Results = append([]ResultRecord(nil), checkpoint.Results...)
	tampered.Results[0].Usage.CostMicroUSD++
	require.Error(t, tampered.Validate())

	duplicate := strings.Replace(
		encoded.String(),
		`"schema_version":1,`,
		`"schema_version":1,"schema_version":1,`,
		1,
	)
	_, err = ReadCampaignRunCheckpoint(strings.NewReader(duplicate))
	require.ErrorContains(t, err, "duplicate object key")
}

func campaignArtifactResults(
	t *testing.T,
) (Corpus, []ResultRecord, CampaignRunBinding) {
	t.Helper()
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:campaign-a", LanguageEnglish),
		testContentRecord("content:campaign-b", LanguageEnglish),
	})
	results := []ResultRecord{
		contentResult(
			corpus,
			"content:campaign-a",
			ContentFilterActionKeep,
			boolPointer(true),
		),
		contentResult(
			corpus,
			"content:campaign-b",
			ContentFilterActionKeep,
			boolPointer(true),
		),
	}
	for index := range results {
		results[index].Usage = Usage{
			RequestID:          "request:campaign-" + string(rune('a'+index)),
			InputTokens:        10 + int64(index),
			OutputTokens:       2,
			CostMicroUSD:       3 + int64(index),
			AccountingComplete: true,
		}
	}
	binding, err := testCampaignPlan().RunBinding(
		strings.Repeat("9", 64),
		"run-candidate-contentfilter",
	)
	require.NoError(t, err)
	binding.SystemID = results[0].System.SystemID
	binding.SystemManifestSHA256 = results[0].ExecutionAudit.ManifestSHA256
	binding.Corpus.SHA256 = corpus.SHA256
	binding.RouteSnapshot.SHA256 =
		results[0].ExecutionAudit.Route.SnapshotSHA256
	for index := range results {
		copy := binding
		results[index].ExecutionAudit.Campaign = &copy
	}
	return corpus, results, binding
}
