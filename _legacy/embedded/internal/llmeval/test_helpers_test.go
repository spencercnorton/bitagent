package llmeval

import "testing"

const testRequestContractSHA = "0000000000000000000000000000000000000000000000000000000000000000"
const testEvaluatorBuildSHA = "1111111111111111111111111111111111111111111111111111111111111111"
const testManifestSHA = "2222222222222222222222222222222222222222222222222222222222222222"
const testRouteSnapshotSHA = "3333333333333333333333333333333333333333333333333333333333333333"
const testRouteSnapshotFetched = "2026-07-24T12:00:00Z"

func testLabel() LabelMetadata {
	return LabelMetadata{
		Provenance:    LabelProvenanceHumanReview,
		Strength:      LabelStrengthGold,
		SourceRef:     "review:fixture",
		PolicyVersion: "policy-v1",
		ReviewerCount: 2,
		HumanReviewProof: &HumanReviewProof{
			Workflow:               ReviewWorkflowContentJunk,
			PolicyID:               "policy:fixture",
			PolicyVersion:          "policy-v1",
			PolicySHA256:           testEvaluatorBuildSHA,
			EvidenceManifestID:     "evidence:fixture",
			EvidenceManifestSHA256: testEvaluatorBuildSHA,
			EvidenceCorpusSHA256:   testEvaluatorBuildSHA,
			SourceSnapshotSHA256:   testEvaluatorBuildSHA,
		},
	}
}

func testReviewProof(
	corpus Corpus,
	workflow ReviewWorkflow,
	policyVersion string,
) HumanReviewProof {
	return HumanReviewProof{
		Workflow:               workflow,
		PolicyID:               "policy:fixture",
		PolicyVersion:          policyVersion,
		PolicySHA256:           testEvaluatorBuildSHA,
		EvidenceManifestID:     "evidence:fixture",
		EvidenceManifestSHA256: testEvaluatorBuildSHA,
		EvidenceCorpusSHA256:   corpus.SHA256,
		SourceSnapshotSHA256:   testEvaluatorBuildSHA,
	}
}

func testExtraction(title string) MatcherExtraction {
	return MatcherExtraction{
		Title:   title,
		Year:    2024,
		Type:    MediaTypeMovie,
		English: EnglishTrackUnknown,
	}
}

func testRecord(id string, task Task) CorpusRecord {
	label := testLabel()
	if task == TaskMatcherExtract || task == TaskMatcherRerank {
		label.HumanReviewProof.Workflow = ReviewWorkflowMatcher
	}
	return CorpusRecord{
		SchemaVersion: SchemaVersion,
		CaseID:        id,
		Task:          task,
		SliceIDs:      []string{"all", "fixture", "suite:natural"},
		GroupID:       "group:" + id,
		Label:         label,
		PrivacyAttestation: &SourcePrivacyAttestation{
			PlanID:               "plan:fixture",
			SourceSnapshotSHA256: testEvaluatorBuildSHA,
			Status:               PrivacyVerifiedPostRulePublic,
		},
	}
}

func testExtractRecord(id string, expected MatcherExtractExpected) CorpusRecord {
	record := testRecord(id, TaskMatcherExtract)
	record.MatcherExtract = &MatcherExtractCase{
		Input: MatcherExtractInput{
			ReleaseName: "Example.Movie.2024.1080p",
			FilePaths:   []string{"Example.Movie.2024.mkv"},
		},
		Expected: expected,
	}

	return record
}

func testRerankRecord(id string, expected MatcherRerankExpected) CorpusRecord {
	record := testRecord(id, TaskMatcherRerank)
	record.MatcherRerank = &MatcherRerankCase{
		Input: MatcherRerankInput{
			ReleaseName:        "Example.Movie.2024.1080p",
			ParsedTitle:        "Example Movie",
			Extraction:         testExtraction("Example Movie"),
			EffectiveMediaType: MediaTypeMovie,
			Candidates: []MatcherCandidate{
				{
					TMDBID:   101,
					Type:     MediaTypeMovie,
					Title:    "Example Movie",
					Year:     2024,
					Overview: "The expected film.",
				},
				{
					TMDBID:   202,
					Type:     MediaTypeMovie,
					Title:    "Example Movie: The Remake",
					Year:     2021,
					Overview: "A different film.",
				},
			},
		},
		Expected: expected,
	}

	return record
}

func testContentRecord(id string, language LanguageClass) CorpusRecord {
	record := testRecord(id, TaskContentFilter)
	record.ContentFilter = &ContentFilterCase{
		Input:    ContentFilterInput{Title: "Example title " + id},
		Expected: ContentFilterExpected{Language: language},
	}

	return record
}

// testJunkRecord takes a CONTENT CLASS, not a disposition: that is the only
// axis a reviewer supplies, and taking the disposition here would let a fixture
// express a combination Validate rejects.
func testJunkRecord(id string, class JunkContentClass) CorpusRecord {
	record := testRecord(id, TaskJunkPurge)
	record.Tier = GoldTierAResolvableKeep
	disposition, err := DeriveJunkDisposition(JunkDispositionPolicyV1, class)
	if err != nil {
		panic(err)
	}
	record.JunkPurge = &JunkPurgeCase{
		Input: JunkPurgeInput{TorrentName: "Example torrent " + id},
		Expected: JunkPurgeExpected{
			Disposition:       disposition,
			ContentClass:      class,
			DispositionPolicy: JunkDispositionPolicyV1,
		},
	}

	return record
}

func testSystem() SystemDescriptor {
	return SystemDescriptor{
		SystemID:      "openrouter:mistral-nemo:deepinfra-fp8",
		Provider:      "openrouter",
		Model:         "mistralai/mistral-nemo",
		Variant:       "deepinfra/fp8",
		PromptVersion: "eval-v1",
	}
}

func testResult(corpus Corpus, id string, task Task) ResultRecord {
	return ResultRecord{
		SchemaVersion:         SchemaVersion,
		CorpusSHA256:          corpus.SHA256,
		RequestContractSHA256: testRequestContractSHA,
		EvaluatorBuildSHA256:  testEvaluatorBuildSHA,
		ExecutionAudit:        testOpenRouterExecutionAudit(),
		CaseID:                id,
		Task:                  task,
		System:                testSystem(),
		Status:                ResultStatusOK,
	}
}

func testExecutionBinding() ExecutionBinding {
	return ExecutionBinding{
		ManifestSHA256:         testManifestSHA,
		RouteSnapshotSHA256:    testRouteSnapshotSHA,
		RouteSnapshotFetchedAt: testRouteSnapshotFetched,
		ExpectedModel:          "test/model",
		ExpectedProvider:       "Test Provider",
	}
}

func testOpenRouterExecutionAudit() ExecutionAudit {
	return ExecutionAudit{
		ManifestSHA256: testManifestSHA,
		Route: RouteAudit{
			SnapshotSHA256:    testRouteSnapshotSHA,
			SnapshotFetchedAt: testRouteSnapshotFetched,
			ReturnedModel:     "test/model",
			ReturnedProvider:  "Test Provider",
			Proof:             RouteProofRouterMetadata,
			Verified:          true,
		},
	}
}

func mustCorpus(t *testing.T, records []CorpusRecord) Corpus {
	t.Helper()
	corpus, err := NewCorpus(records)
	if err != nil {
		t.Fatalf("NewCorpus: %v", err)
	}

	return corpus
}

func boolPointer(value bool) *bool {
	return &value
}
