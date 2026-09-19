package llmeval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildProductionCandidateExportIsDeterministicAndNotGold(t *testing.T) {
	plan := testCorpusPlan()
	rawPlan, err := json.Marshal(plan)
	require.NoError(t, err)
	_, planSHA, err := ReadCorpusPlan(bytes.NewReader(rawPlan))
	require.NoError(t, err)

	snapshot := testProductionSourceSnapshot()
	first, err := BuildProductionCandidateExport(plan, planSHA, snapshot)
	require.NoError(t, err)

	reversed := snapshot
	reversed.Seeds = append([]ProductionCandidateSeed(nil), snapshot.Seeds...)
	for left, right := 0, len(reversed.Seeds)-1; left < right; left, right =
		left+1, right-1 {
		reversed.Seeds[left], reversed.Seeds[right] =
			reversed.Seeds[right], reversed.Seeds[left]
	}
	second, err := BuildProductionCandidateExport(plan, planSHA, reversed)
	require.NoError(t, err)

	require.Equal(t, first.Corpus.SHA256, second.Corpus.SHA256)
	require.Equal(t, first.Manifest, second.Manifest)
	require.Equal(t, first.PrivacySidecar, second.PrivacySidecar)
	require.Equal(t, ProductionCandidateExportStatus, first.Manifest.Status)
	require.True(t, first.Manifest.FreezeCompatibilityChecked)
	require.Len(t, first.Manifest.Tasks, len(orderedTasks))
	for _, task := range first.Manifest.Tasks {
		require.Equal(t, int64(20), task.CohortCounts.EligibleBeforePrivacy)
		require.Equal(t, int64(2), task.CohortCounts.NativePrivateExcluded)
		require.Equal(t, int64(1), task.CohortCounts.PrivateEvidenceExcluded)
		require.Equal(t, int64(17), task.CohortCounts.PrivacySafeAvailable)
		require.Equal(t, int64(12), task.CohortCounts.SourceRowsRead)
		require.Equal(
			t,
			int64(12),
			task.CohortCounts.DeduplicatedCandidateCases,
		)
		require.Equal(t, int64(6), task.CohortCounts.RowsByStratum["natural"])
	}
	for _, record := range first.Corpus.Records {
		require.Equal(
			t,
			LabelProvenanceSamplingCandidate,
			record.Label.Provenance,
		)
		require.Equal(t, LabelStrengthUnreviewed, record.Label.Strength)
		require.NotContains(t, record.CaseID, "raw-source")
		require.NotContains(t, record.GroupID, "raw-group")
	}
	sidecarRaw, err := MarshalProductionPrivacySidecar(first.PrivacySidecar)
	require.NoError(t, err)
	require.Equal(t, first.Manifest.PrivacySidecarSHA256, sha256Hex(sidecarRaw))
	require.Len(t, first.PrivacySidecar.Entries, len(first.Corpus.Records))

	var artifact bytes.Buffer
	_, err = WriteCorpus(&artifact, first.Corpus.Records)
	require.NoError(t, err)
	require.NotContains(
		t,
		artifact.String(),
		first.PrivacySidecar.Entries[0].InfoHashHex,
	)
	require.NotContains(t, artifact.String(), "raw-group")
	manifestArtifact, err := json.Marshal(first.Manifest)
	require.NoError(t, err)
	require.NotContains(
		t,
		string(manifestArtifact),
		first.PrivacySidecar.Entries[0].InfoHashHex,
	)
}

func TestDiagnosticProductionExportCannotClaimVerifiedFreeze(t *testing.T) {
	plan := testCorpusPlan()
	plan.Readiness.ProductionSourceExporter = true
	rawPlan, err := json.Marshal(plan)
	require.NoError(t, err)
	_, planSHA, err := ReadCorpusPlan(bytes.NewReader(rawPlan))
	require.NoError(t, err)
	exported, err := BuildProductionCandidateExport(
		plan,
		planSHA,
		testProductionSourceSnapshot(),
	)
	require.NoError(t, err)
	rawManifest, err := json.Marshal(exported.Manifest)
	require.NoError(t, err)
	decoded, manifestSHA, err := ReadProductionCandidateExportManifest(
		bytes.NewReader(rawManifest),
	)
	require.NoError(t, err)
	require.Equal(t, exported.Manifest, decoded)

	_, err = FreezeProductionCandidateCorpus(
		plan,
		planSHA,
		exported.Corpus,
		decoded,
		manifestSHA,
		exported.PrivacySidecar,
		exported.Manifest.PrivacySidecarSHA256,
	)
	require.ErrorContains(t, err, "only the audited natural-capture")

	unbound, err := FreezeCandidateCorpus(plan, planSHA, exported.Corpus)
	require.NoError(t, err)
	require.ErrorContains(
		t,
		ValidateCorpusFreezeManifest(
			plan,
			planSHA,
			exported.Corpus,
			unbound.Manifest,
		),
		"must bind a verified production export manifest",
	)

	tamperedManifest := decoded
	tamperedManifest.ReadOnlyVerified = false
	_, err = FreezeProductionCandidateCorpus(
		plan,
		planSHA,
		exported.Corpus,
		tamperedManifest,
		manifestSHA,
		exported.PrivacySidecar,
		exported.Manifest.PrivacySidecarSHA256,
	)
	require.ErrorContains(t, err, "read-only repeatable-read")

	tamperedRecords := append([]CorpusRecord(nil), exported.Corpus.Records...)
	tamperedRecords[0].Label = LabelMetadata{
		Provenance:    LabelProvenanceProductionTeacher,
		Strength:      LabelStrengthTeacher,
		PolicyVersion: "teacher-v1",
	}
	tampered, err := NewCorpus(tamperedRecords)
	require.NoError(t, err)
	tamperedManifest = decoded
	tamperedManifest.CandidateCorpusSHA256 = tampered.SHA256
	tamperedManifest.CandidateCases = len(tampered.Records)
	err = ValidateProductionCandidateExportManifest(
		plan,
		planSHA,
		tampered,
		tamperedManifest,
	)
	require.ErrorContains(t, err, "unreviewed sampling placeholder")
}

func TestProductionPrivacySidecarRejectsTamperMissingAndMismatch(t *testing.T) {
	plan := testCorpusPlan()
	rawPlan, err := json.Marshal(plan)
	require.NoError(t, err)
	_, planSHA, err := ReadCorpusPlan(bytes.NewReader(rawPlan))
	require.NoError(t, err)
	exported, err := BuildProductionCandidateExport(
		plan,
		planSHA,
		testProductionSourceSnapshot(),
	)
	require.NoError(t, err)

	var raw bytes.Buffer
	writtenSHA, err := WriteProductionPrivacySidecar(
		&raw,
		exported.PrivacySidecar,
	)
	require.NoError(t, err)
	require.Equal(t, exported.Manifest.PrivacySidecarSHA256, writtenSHA)
	decoded, exactSHA, err := ReadProductionPrivacySidecar(&raw)
	require.NoError(t, err)
	require.NoError(t, ValidateProductionPrivacySidecar(
		plan,
		planSHA,
		exported.Corpus,
		exported.Manifest,
		decoded,
		exactSHA,
	))

	tampered := decoded
	tampered.Entries[0].InfoHashHex = strings.Repeat("f", 40)
	tamperedRaw, err := MarshalProductionPrivacySidecar(tampered)
	require.NoError(t, err)
	_, tamperedSHA, err := ReadProductionPrivacySidecar(
		bytes.NewReader(tamperedRaw),
	)
	require.NoError(t, err)
	require.ErrorContains(t, ValidateProductionPrivacySidecar(
		plan,
		planSHA,
		exported.Corpus,
		exported.Manifest,
		tampered,
		tamperedSHA,
	), "do not match the export manifest")

	missing := decoded
	missing.Entries = missing.Entries[:len(missing.Entries)-1]
	require.ErrorContains(t, ValidateProductionPrivacySidecar(
		plan,
		planSHA,
		exported.Corpus,
		exported.Manifest,
		missing,
		exactSHA,
	), "entries")

	mismatchedManifest := exported.Manifest
	mismatchedManifest.PrivacySidecarSHA256 = strings.Repeat("0", 64)
	require.ErrorContains(t, ValidateProductionPrivacySidecar(
		plan,
		planSHA,
		exported.Corpus,
		mismatchedManifest,
		decoded,
		exactSHA,
	), "do not match the export manifest")
}

func TestProductionExportManifestReaderRejectsUnknownAndTrailingData(t *testing.T) {
	_, _, err := ReadProductionCandidateExportManifest(
		strings.NewReader(`{"unknown":true}`),
	)
	require.ErrorContains(t, err, "unknown field")

	_, _, err = ReadProductionCandidateExportManifest(
		strings.NewReader(`{} {}`),
	)
	require.Error(t, err)
}

func TestSamplingCandidatesCannotRunScoreOrCompare(t *testing.T) {
	plan := testCorpusPlan()
	rawPlan, err := json.Marshal(plan)
	require.NoError(t, err)
	_, planSHA, err := ReadCorpusPlan(bytes.NewReader(rawPlan))
	require.NoError(t, err)
	exported, err := BuildProductionCandidateExport(
		plan,
		planSHA,
		testProductionSourceSnapshot(),
	)
	require.NoError(t, err)

	_, err = RunCorpus(
		context.Background(),
		exported.Corpus,
		SystemConfig{},
		"",
		nil,
		RunOptions{},
	)
	require.ErrorContains(t, err, "unreviewed sampling candidate")

	_, err = Score(exported.Corpus, nil, testEvaluatorBuildSHA)
	require.ErrorContains(t, err, "unreviewed sampling candidate")

	_, err = Compare(
		exported.Corpus,
		nil,
		nil,
		ComparisonOptions{},
	)
	require.ErrorContains(t, err, "unreviewed sampling candidate")
}

func TestSamplingCandidatePlaceholderStateIsClosed(t *testing.T) {
	record := testFreezeRecord(
		"candidate:closed",
		"group:candidate:closed",
		TaskContentFilter,
	)
	record.Label = LabelMetadata{
		Provenance:    LabelProvenanceSamplingCandidate,
		Strength:      LabelStrengthUnreviewed,
		SourceRef:     "candidate:natural",
		PolicyVersion: CandidatePlaceholderPolicyVersion,
	}
	record.ContentFilter.Expected = ContentFilterExpected{AllowAbstain: true}
	require.NoError(t, record.Validate())

	withFakeTruth := record
	withFakeTruth.ContentFilter.Expected = ContentFilterExpected{
		Language: LanguageEnglish,
	}
	require.ErrorContains(
		t,
		withFakeTruth.Validate(),
		"abstain-only placeholder",
	)

	wrongStrength := record
	wrongStrength.Label.Strength = LabelStrengthTeacher
	require.ErrorContains(
		t,
		wrongStrength.Validate(),
		"requires \"unreviewed\"",
	)

	nonCandidate := record
	nonCandidate.Label.Provenance = LabelProvenanceProductionTeacher
	require.ErrorContains(
		t,
		nonCandidate.Validate(),
		"allowed only for sampling_candidate",
	)
}

func TestCandidateReviewAssignmentDoesNotExposePlaceholder(t *testing.T) {
	plan := testCorpusPlan()
	rawPlan, err := json.Marshal(plan)
	require.NoError(t, err)
	_, planSHA, err := ReadCorpusPlan(bytes.NewReader(rawPlan))
	require.NoError(t, err)
	exported, err := BuildProductionCandidateExport(
		plan,
		planSHA,
		testProductionSourceSnapshot(),
	)
	require.NoError(t, err)
	proof := testReviewProof(
		exported.Corpus,
		ReviewWorkflowContentJunk,
		"review-policy-v1",
	)
	assignment, err := NewReviewAssignment(
		exported.Corpus,
		ReviewAssignmentSpec{
			ReviewSetID:   "review-set:candidate",
			ReviewerID:    "reviewer:a",
			PolicyVersion: proof.PolicyVersion,
			ReviewProof:   proof,
			Tasks:         []Task{TaskContentFilter, TaskJunkPurge},
		},
	)
	require.NoError(t, err)
	raw, err := json.Marshal(assignment.Cases)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "expected")
	require.NotContains(t, string(raw), CandidatePlaceholderPolicyVersion)
	require.NotContains(t, string(raw), string(LabelStrengthUnreviewed))
	require.NotContains(t, string(raw), string(JunkVerdictUnsure))
}

func TestProductionExportFailsClosedOnSnapshotAndPrivacyClaims(t *testing.T) {
	plan := testCorpusPlan()
	rawPlan, err := json.Marshal(plan)
	require.NoError(t, err)
	_, planSHA, err := ReadCorpusPlan(bytes.NewReader(rawPlan))
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*ProductionSourceSnapshot)
		want   string
	}{
		{
			name: "not read only",
			mutate: func(snapshot *ProductionSourceSnapshot) {
				snapshot.ReadOnlyVerified = false
			},
			want: "read-only",
		},
		{
			name: "not repeatable read",
			mutate: func(snapshot *ProductionSourceSnapshot) {
				snapshot.RepeatableReadVerified = false
			},
			want: "repeatable-read",
		},
		{
			name: "model calls not disabled",
			mutate: func(snapshot *ProductionSourceSnapshot) {
				snapshot.NoModelCalls = false
			},
			want: "model calls were disabled",
		},
		{
			name: "unsafe row",
			mutate: func(snapshot *ProductionSourceSnapshot) {
				snapshot.Seeds[0].PrivacySafe = false
			},
			want: "privacy gate was not verified",
		},
		{
			name: "missing cohort accounting",
			mutate: func(snapshot *ProductionSourceSnapshot) {
				delete(snapshot.TaskCohortCounts, TaskContentFilter)
			},
			want: "has no \"contentfilter\" cohort counts",
		},
		{
			name: "candidate count mismatch",
			mutate: func(snapshot *ProductionSourceSnapshot) {
				counts := snapshot.TaskCohortCounts[TaskJunkPurge]
				counts.DeduplicatedCandidateCases--
				snapshot.TaskCohortCounts[TaskJunkPurge] = counts
			},
			want: "candidate count does not match",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := testProductionSourceSnapshot()
			test.mutate(&snapshot)
			_, err := BuildProductionCandidateExport(
				plan,
				planSHA,
				snapshot,
			)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func testProductionSourceSnapshot() ProductionSourceSnapshot {
	querySHAs := make(map[Task]string, len(orderedTasks))
	cohortCounts := make(map[Task]ProductionSourceCohortCounts, len(orderedTasks))
	for i, task := range orderedTasks {
		querySHAs[task] = fmt.Sprintf("%064x", i+10)
		cohortCounts[task] = ProductionSourceCohortCounts{
			EligibleBeforePrivacy:      20,
			NativePrivateExcluded:      2,
			PrivateEvidenceExcluded:    1,
			PrivacySafeAvailable:       17,
			SourceRowsRead:             12,
			DeduplicatedCandidateCases: 12,
			RowsByStratum: map[string]int64{
				"natural": 6,
				"safety":  6,
			},
		}
	}
	var seeds []ProductionCandidateSeed
	infoHashOrdinal := 1
	for _, task := range orderedTasks {
		suites := []string{"natural", "safety"}
		if task == TaskJunkPurge {
			suites = []string{"natural", "safety_top_up"}
		}
		for suiteIndex, suite := range suites {
			for groupIndex := 0; groupIndex < 3; groupIndex++ {
				for caseIndex := 0; caseIndex < 2; caseIndex++ {
					seed := ProductionCandidateSeed{
						Task:      task,
						SourceKey: fmt.Sprintf("%040x", infoHashOrdinal),
						GroupKey: fmt.Sprintf(
							"raw-group:%s:%d:%d",
							task,
							suiteIndex,
							groupIndex,
						),
						PrimarySuite: suite,
						Stratum:      "natural",
						PrivacySafe:  true,
					}
					infoHashOrdinal++
					if suite == "safety" {
						seed.SliceIDs = append(
							seed.SliceIDs,
							"required-risk",
							"hard_keep",
						)
						seed.Stratum = "safety"
					}
					if task == TaskJunkPurge {
						seed.SliceIDs = append(
							seed.SliceIDs,
							"verified_real",
						)
						if suite == "safety_top_up" {
							seed.Stratum = "teacher-real"
						}
					}
					switch task {
					case TaskMatcherExtract:
						seed.MatcherExtract = &MatcherExtractInput{
							ReleaseName: fmt.Sprintf(
								"Example.Movie.%d.%d",
								groupIndex,
								caseIndex,
							),
						}
					case TaskMatcherRerank:
						seed.MatcherRerank = &MatcherRerankInput{
							ReleaseName: "Example.Movie.2024",
							Extraction:  testExtraction("Example Movie"),
							Candidates: []MatcherCandidate{{
								TMDBID: int64(
									100 + groupIndex*10 + caseIndex,
								),
								Type:  MediaTypeMovie,
								Title: "Example Movie",
								Year:  2024,
							}},
						}
					case TaskContentFilter:
						seed.ContentFilter = &ContentFilterInput{
							Title: fmt.Sprintf(
								"Example title %d %d",
								groupIndex,
								caseIndex,
							),
						}
					case TaskJunkPurge:
						seed.JunkPurge = &JunkPurgeInput{
							TorrentName: fmt.Sprintf(
								"Example torrent %d %d",
								groupIndex,
								caseIndex,
							),
						}
					}
					seeds = append(seeds, seed)
				}
			}
		}
	}
	return ProductionSourceSnapshot{
		ObservedAtUTC:             "2026-07-24T18:00:00Z",
		DatabaseIdentitySHA256:    strings.Repeat("a", 64),
		TransactionSnapshotSHA256: strings.Repeat("b", 64),
		QueryBundleSHA256:         strings.Repeat("c", 64),
		TaskQuerySHA256:           querySHAs,
		ReadOnlyVerified:          true,
		RepeatableReadVerified:    true,
		NoModelCalls:              true,
		Seeds:                     seeds,
		TaskCohortCounts:          cohortCounts,
		TaskLimitations: map[Task][]string{
			TaskMatcherRerank: {
				"fixture local-mirror candidates only",
			},
		},
	}
}
