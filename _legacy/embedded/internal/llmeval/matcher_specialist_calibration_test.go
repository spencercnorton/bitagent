package llmeval

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMatcherSpecialistCalibrationPreservesPolicyVeto(t *testing.T) {
	natural := testRerankRecord(
		"specialist-natural",
		MatcherRerankExpected{AcceptableTMDBIDs: []int64{101}},
	)
	safety := testRerankRecord(
		"specialist-safety",
		MatcherRerankExpected{AcceptableTMDBIDs: []int64{101}},
	)
	safety.SliceIDs = []string{"all", "fixture", "suite:safety"}
	// Keep the wrong candidate specialist-eligible so only the shared final
	// title gate prevents the attachment.
	natural.MatcherRerank.Input.Candidates[1].Year = 2024
	safety.MatcherRerank.Input.Candidates[1].Year = 2024
	development := mustCorpus(t, []CorpusRecord{natural, safety})

	system := SystemConfig{
		SystemID:         "openrouter-specialist-test",
		Provider:         "openrouter",
		Model:            "openai/text-embedding-3-small",
		Variant:          "openai",
		PromptVersion:    MatcherSpecialistAlgorithmID,
		EvaluationLane:   EvaluationLaneSpecialist,
		APIKind:          APIKindEmbedding,
		OutputContract:   OutputContractNative,
		BaseURL:          "https://openrouter.ai/api/v1",
		APIKeyEnv:        "OPENROUTER_API_KEY",
		ProviderEndpoint: "openai",
		Tasks:            []Task{TaskMatcherRerank},
		ZDR:              true,
		NoThinkLocation:  NoThinkLocationNone,
		RequestTimeoutMS: 60_000,
	}
	require.NoError(t, system.Validate())

	closureSHA := "4444444444444444444444444444444444444444444444444444444444444444"
	closure := GoldDevelopmentClosureManifest{
		SchemaVersion:                 SchemaVersion,
		Status:                        GoldDevelopmentClosureStatus,
		SelectionAlgorithmID:          GoldDevelopmentClosureAlgorithmID,
		PlanID:                        "plan:specialist-calibration-test",
		PlanSHA256:                    "5555555555555555555555555555555555555555555555555555555555555555",
		CandidateCorpusSHA256:         "6666666666666666666666666666666666666666666666666666666666666666",
		CandidateFreezeManifestSHA256: "7777777777777777777777777777777777777777777777777777777777777777",
		CandidateDevelopmentSHA256:    "8888888888888888888888888888888888888888888888888888888888888888",
		CandidateDevelopmentCases:     len(development.Records),
		DevelopmentCorpusSHA256:       development.SHA256,
		DevelopmentCases:              len(development.Records),
		Tasks: []GoldDevelopmentTaskSummary{{
			Task: TaskMatcherRerank, SelectedCases: 2, SelectedGroups: 2,
		}},
	}

	zeroThresholds, err := MatcherSpecialistThresholds(
		ProductionThresholds(),
		0,
	)
	require.NoError(t, err)
	makeResult := func(
		record CorpusRecord,
		selectedID int64,
	) ResultRecord {
		t.Helper()
		audit := &MatcherSpecialistResultAudit{
			AlgorithmID:        MatcherSpecialistAlgorithmID,
			SelectedTMDBID:     selectedID,
			ScorePPB:           MatcherSpecialistScoreScalePPB,
			EligibleCandidates: 2,
		}
		result := ParseCompletion(
			development.SHA256,
			record,
			system,
			Completion{
				Text: []byte(fmt.Sprintf(
					`{"tmdb_id":%d,"confidence":1}`,
					selectedID,
				)),
				MatcherSpecialist: audit,
			},
			zeroThresholds,
		)
		require.Equal(t, ResultStatusOK, result.Status)
		result.EvaluatorBuildSHA256 = testEvaluatorBuildSHA
		result.ExecutionAudit = testOpenRouterExecutionAudit()
		result.ExecutionAudit.Route.ReturnedModel = system.Model
		result.ExecutionAudit.Route.ReturnedProvider = "OpenAI"
		result.ExecutionAudit.MatcherSpecialistDevelopment =
			&MatcherSpecialistDevelopmentExecutionAudit{
				Mode:                             MatcherSpecialistDevelopmentRaw,
				DevelopmentClosureManifestSHA256: closureSHA,
				ThresholdPPB:                     0,
			}
		return result
	}
	rawResults := []ResultRecord{
		makeResult(natural, 101),
		makeResult(safety, 202),
	}
	require.Equal(t, MatcherRerankActionAttach, rawResults[0].MatcherRerank.Action)
	require.Equal(t, MatcherRerankActionAbstain, rawResults[1].MatcherRerank.Action)
	require.Equal(t, "candidate_title", rawResults[1].MatcherRerank.PolicyReason)

	artifact, err := FitMatcherSpecialistCalibration(
		MatcherSpecialistCalibrationInput{
			Development:              development,
			DevelopmentClosure:       closure,
			DevelopmentClosureSHA256: closureSHA,
			System:                   system,
			SystemManifestSHA256:     testManifestSHA,
			RouteSnapshotSHA256:      testRouteSnapshotSHA,
			EvaluatorBuildSHA256:     testEvaluatorBuildSHA,
			RawResults:               rawResults,
			RawResultsArtifactSHA256: "9999999999999999999999999999999999999999999999999999999999999999",
		},
	)
	require.NoError(t, err)
	require.Zero(t, artifact.ThresholdPPB)
	require.Equal(t, 1, artifact.After.AttachmentAttempts)
	require.Equal(t, 1, artifact.After.CorrectAttachments)
	require.Zero(t, artifact.After.WrongAttachments)
	require.Equal(t, 1, artifact.After.Abstentions)

	rethresholded, err := RethresholdMatcherSpecialistDevelopment(
		development,
		system,
		rawResults,
		artifact,
	)
	require.NoError(t, err)
	byCase := make(map[string]ResultRecord, len(rethresholded))
	for _, result := range rethresholded {
		byCase[result.CaseID] = result
	}
	require.Equal(
		t,
		MatcherRerankActionAttach,
		byCase[natural.CaseID].MatcherRerank.Action,
	)
	require.Equal(
		t,
		MatcherRerankActionAbstain,
		byCase[safety.CaseID].MatcherRerank.Action,
	)
	require.Equal(
		t,
		"candidate_title",
		byCase[safety.CaseID].MatcherRerank.PolicyReason,
	)
}
