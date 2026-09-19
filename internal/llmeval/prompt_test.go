package llmeval

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildPromptUsesFrozenMatcherInput(t *testing.T) {
	record := CorpusRecord{
		CaseID: "rerank-1",
		Task:   TaskMatcherRerank,
		MatcherRerank: &MatcherRerankCase{
			Input: MatcherRerankInput{
				ReleaseName: "Example.2024.1080p",
				Extraction: MatcherExtraction{
					Title: "Example", Year: 2024, Type: MediaTypeMovie,
					English: EnglishTrackUnknown,
				},
				Candidates: []MatcherCandidate{
					{TMDBID: 42, Type: MediaTypeMovie, Title: "Example", Year: 2024},
				},
			},
		},
	}

	request, err := BuildPrompt(record)
	require.NoError(t, err)
	require.Equal(t, TaskMatcherRerank, request.Task)
	require.Contains(t, request.System, "wrong match is worse")
	require.Contains(t, request.User, "tmdb_id=42")
	require.Equal(t, false, request.Schema["additionalProperties"])
}

func TestPromptIdentityIncludesRenderedCaseInput(t *testing.T) {
	first, err := BuildPrompt(CorpusRecord{
		CaseID: "a", Task: TaskContentFilter,
		ContentFilter: &ContentFilterCase{Input: ContentFilterInput{Title: "First"}},
	})
	require.NoError(t, err)
	second, err := BuildPrompt(CorpusRecord{
		CaseID: "b", Task: TaskContentFilter,
		ContentFilter: &ContentFilterCase{Input: ContentFilterInput{Title: "Second"}},
	})
	require.NoError(t, err)

	firstBytes, err := PromptSHA256Input(first)
	require.NoError(t, err)
	secondBytes, err := PromptSHA256Input(second)
	require.NoError(t, err)
	require.NotEqual(t, sha256.Sum256(firstBytes), sha256.Sum256(secondBytes))
}

func TestRequestContractIdentityCoversRenderedInputRouteAndThresholds(t *testing.T) {
	record := contentRecord("content-1", "First")
	system := testOpenRouterSystem(TaskContentFilter)
	thresholds := ProductionThresholds()

	baseline, err := RequestContractSHA256(record, system, thresholds)
	require.NoError(t, err)

	changedInput := record
	changedContent := *record.ContentFilter
	changedContent.Input.Title = "Second"
	changedInput.ContentFilter = &changedContent
	inputDigest, err := RequestContractSHA256(changedInput, system, thresholds)
	require.NoError(t, err)
	require.NotEqual(t, baseline, inputDigest)

	changedRoute := system
	changedRoute.ProviderEndpoint = "other/fp8"
	routeDigest, err := RequestContractSHA256(record, changedRoute, thresholds)
	require.NoError(t, err)
	require.NotEqual(t, baseline, routeDigest)

	changedTimeout := system
	changedTimeout.RequestTimeoutMS = 8_000
	timeoutDigest, err := RequestContractSHA256(
		record,
		changedTimeout,
		thresholds,
	)
	require.NoError(t, err)
	require.NotEqual(t, baseline, timeoutDigest)

	changedThresholds := thresholds
	changedThresholds.ContentDropConfidence = 0.90
	thresholdDigest, err := RequestContractSHA256(record, system, changedThresholds)
	require.NoError(t, err)
	require.NotEqual(t, baseline, thresholdDigest)
}

func TestRequestContractIdentityIncludesSpecializedRerankDocuments(t *testing.T) {
	record := testRerankRecord("rerank-1", MatcherRerankExpected{
		AcceptableTMDBIDs: []int64{101},
	})
	system := testOpenRouterSystem(TaskMatcherRerank)
	baseline, err := RequestContractSHA256(
		record,
		system,
		ProductionThresholds(),
	)
	require.NoError(t, err)

	changed := record
	changedCase := *record.MatcherRerank
	changedInput := record.MatcherRerank.Input
	changedInput.Candidates = append(
		[]MatcherCandidate(nil),
		record.MatcherRerank.Input.Candidates...,
	)
	// OriginalTitle is rendered only into the specialist document, not the
	// ordinary chat candidate passed to RerankInput.
	changedInput.Candidates[0].OriginalTitle = "Specialist-only mutation"
	changedCase.Input = changedInput
	changed.MatcherRerank = &changedCase

	mutated, err := RequestContractSHA256(
		changed,
		system,
		ProductionThresholds(),
	)
	require.NoError(t, err)
	require.NotEqual(t, baseline, mutated)
}
