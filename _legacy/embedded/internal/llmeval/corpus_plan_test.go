package llmeval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadCorpusPlanIsStrictAndBindsExactBytes(t *testing.T) {
	plan := testCorpusPlan()
	raw, err := json.Marshal(plan)
	require.NoError(t, err)

	decoded, sha, err := ReadCorpusPlan(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, plan.PlanID, decoded.PlanID)
	require.Len(t, sha, 64)

	_, shaWithWhitespace, err := ReadCorpusPlan(
		bytes.NewReader(append(append([]byte(nil), raw...), '\n')),
	)
	require.NoError(t, err)
	require.NotEqual(t, sha, shaWithWhitespace)

	_, _, err = ReadCorpusPlan(strings.NewReader(
		`{"schema_version":1,"schema_version":1}`,
	))
	require.ErrorContains(t, err, "duplicate object key")

	var withUnknown map[string]any
	require.NoError(t, json.Unmarshal(raw, &withUnknown))
	withUnknown["unexpected"] = true
	unknownRaw, err := json.Marshal(withUnknown)
	require.NoError(t, err)
	_, _, err = ReadCorpusPlan(bytes.NewReader(unknownRaw))
	require.ErrorContains(t, err, "unknown field")

	_, _, err = ReadCorpusPlan(bytes.NewReader(append(raw, []byte(`{}`)...)))
	require.Error(t, err)
}

func TestCorpusPlanRejectsInconsistentReadinessAndAllocation(t *testing.T) {
	plan := testCorpusPlan()
	plan.Readiness.MachinePlanValidator = false
	require.ErrorContains(t, plan.Validate(), "machine_plan_validator")

	plan = testCorpusPlan()
	plan.Tasks[0].HoldoutAllocation["natural"]++
	require.ErrorContains(t, plan.Validate(), "totals")

	plan = testCorpusPlan()
	plan.Executable = true
	plan.NonExecutableReason = ""
	require.ErrorContains(t, plan.Validate(), "production_source_exporter")
}

func TestFreezeCandidateCorpusIsDeterministicAndGroupPreserving(t *testing.T) {
	plan := testCorpusPlan()
	candidates, err := NewCorpus(testFreezeCandidateRecords(plan.PlanID))
	require.NoError(t, err)
	planRaw, err := json.Marshal(plan)
	require.NoError(t, err)
	_, planSHA, err := ReadCorpusPlan(bytes.NewReader(planRaw))
	require.NoError(t, err)

	first, err := FreezeCandidateCorpus(plan, planSHA, candidates)
	require.NoError(t, err)

	reversed := append([]CorpusRecord(nil), candidates.Records...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	reordered, err := NewCorpus(reversed)
	require.NoError(t, err)
	second, err := FreezeCandidateCorpus(plan, planSHA, reordered)
	require.NoError(t, err)

	require.Equal(t, first.Development.SHA256, second.Development.SHA256)
	require.Equal(t, first.Holdout.SHA256, second.Holdout.SHA256)
	require.Equal(t, first.Manifest, second.Manifest)
	require.Equal(t, CorpusFreezeStatus, first.Manifest.Status)
	require.False(t, first.Manifest.PlanExecutable)
	require.False(t, first.Manifest.ProductionSourceExporterVerified)
	require.Len(t, first.Development.Records, 8)
	require.Len(t, first.Holdout.Records, 16)

	developmentGroups := make(map[string]struct{})
	for _, record := range first.Development.Records {
		developmentGroups[string(record.Task)+"\x00"+record.GroupID] = struct{}{}
	}
	for _, record := range first.Holdout.Records {
		_, overlaps := developmentGroups[string(record.Task)+"\x00"+record.GroupID]
		require.False(t, overlaps)
	}
	for _, summary := range first.Manifest.Tasks {
		require.Equal(t, 2, summary.DevelopmentCases)
		require.Equal(t, 2, summary.DevelopmentGroups)
		require.Equal(t, 4, summary.HoldoutCases)
		require.Equal(t, 2, summary.HoldoutGroups)
		for _, allocation := range summary.HoldoutAllocations {
			require.GreaterOrEqual(t, allocation.ActualCases, allocation.MinimumCases)
		}
	}
}

func TestFreezeCandidateCorpusFailsClosedOnPrivacyAndSuiteErrors(t *testing.T) {
	plan := testCorpusPlan()
	records := testFreezeCandidateRecords(plan.PlanID)
	records[0].PrivacyAttestation.PlanID = "different-plan"
	candidates, err := NewCorpus(records)
	require.NoError(t, err)
	_, err = FreezeCandidateCorpus(plan, strings.Repeat("a", 64), candidates)
	require.ErrorContains(t, err, "does not match")

	records = testFreezeCandidateRecords(plan.PlanID)
	records[0].SliceIDs = []string{"required-risk"}
	candidates, err = NewCorpus(records)
	require.NoError(t, err)
	_, err = FreezeCandidateCorpus(plan, strings.Repeat("a", 64), candidates)
	require.ErrorContains(t, err, "exactly one")

	records = testFreezeCandidateRecords(plan.PlanID)
	groupID := records[0].GroupID
	for i := range records {
		if records[i].Task == records[0].Task &&
			records[i].GroupID == groupID &&
			i != 0 {
			records[i].SliceIDs = []string{"required-risk", "suite:safety"}
			break
		}
	}
	candidates, err = NewCorpus(records)
	require.NoError(t, err)
	_, err = FreezeCandidateCorpus(plan, strings.Repeat("a", 64), candidates)
	require.ErrorContains(t, err, "spans primary suites")

	plan = testCorpusPlan()
	plan.Tasks[2].RequiredSafetyCounts["latin_non_english"] = 1
	records = testFreezeCandidateRecords(plan.PlanID)
	for i := range records {
		if records[i].Task == TaskContentFilter &&
			containsString(records[i].SliceIDs, "suite:safety") {
			records[i].SliceIDs = append(
				records[i].SliceIDs,
				"latin_non_english",
			)
			break
		}
	}
	candidates, err = NewCorpus(records)
	require.NoError(t, err)
	_, err = FreezeCandidateCorpus(plan, strings.Repeat("a", 64), candidates)
	require.ErrorContains(
		t,
		err,
		"required_safety_counts slices must be mutually exclusive",
	)
}

func testCorpusPlan() CorpusPlan {
	plan := CorpusPlan{
		SchemaVersion:       SchemaVersion,
		PlanID:              "plan:test-freeze-v1",
		Status:              "validated_candidate_split_source_export_and_gold_closure_not_implemented",
		Executable:          false,
		NonExecutableReason: "The production source exporter is not implemented.",
		AsOfUTC:             "2026-07-24T16:25:28Z",
		Purpose:             "Test deterministic corpus sampling.",
		NonGoals:            []string{"This plan is not a production export."},
		PrivacyGate: CorpusPlanPrivacyGate{
			Stage: "After source privacy filtering.",
			Requirements: []string{
				"Every record carries a matching privacy attestation.",
			},
		},
		Selection: CorpusPlanSelection{
			Unit:                       "group_id",
			AlgorithmID:                CorpusSplitAlgorithmGroupSHA256V2,
			PrimarySuiteSlicePrefix:    CorpusPrimarySuiteSlicePrefix,
			Deduplication:              "Keep related records in one group.",
			CanonicalOrder:             "Task then case ID.",
			SplitAlgorithm:             "Hash plan, task, and group and select prefixes.",
			DevelopmentTargetSemantics: "Exact independent groups with one deterministic representative case per development group.",
			HoldoutTargetSemantics:     "Minimum suite prefixes with safety top-up.",
			DevelopmentGroups:          "Development is selected before holdout.",
			HoldoutGroups:              "Holdout uses remaining groups.",
			CrossSuiteRule:             "Each group has exactly one suite.",
			ReplacementRule:            "Use the next group in hash order.",
			StageZero:                  "Protocol validation only.",
		},
		Readiness: CorpusPlanReadiness{
			SamplingContract:                true,
			MachinePlanValidator:            true,
			DeterministicSplitFreeze:        true,
			StrictCorpusSchema:              true,
			BlindedTwoReviewerWorkflow:      true,
			IndependentAdjudicationWorkflow: true,
			GoldPromotionWorkflow:           true,
			NextBlockingSteps: []string{
				"Implement the production source exporter.",
			},
		},
	}
	plan.SourceObservations.Classification = "test_observation"
	plan.SourceObservations.ObservedAtUTC = "2026-07-24T16:25:28Z"
	plan.SourceObservations.FrozenSegments = append(
		plan.SourceObservations.FrozenSegments,
		struct {
			Segment             string `json:"segment"`
			Records             int64  `json:"records"`
			RecordsWithExpected int64  `json:"records_with_expected"`
		}{
			Segment: "fixture",
			Records: 1,
		},
	)
	plan.SourceObservations.MatcherEvidence.Caveat = "Test evidence only."
	plan.SourceObservations.JunkTeacherStrata.Caveat = "Test strata only."
	plan.Tasks = []CorpusPlanTask{
		{
			Task:               TaskMatcherExtract,
			SourceCohort:       "Privacy-filtered matcher extraction candidates.",
			ModelVisibleFields: []string{"release_name"},
			DevelopmentTarget:  2,
			DevelopmentAllocation: map[string]int{
				"natural": 1, "safety": 1,
			},
			DevelopmentReplacementReserve: map[string]int{
				"natural": 1, "safety": 1,
			},
			HoldoutTarget:      4,
			HoldoutAllocation:  map[string]int{"natural": 2, "safety": 2},
			ReplacementReserve: map[string]int{"natural": 1, "safety": 1},
			ReserveMaximum:     map[string]int{"natural": 3, "safety": 3},
			RequiredGoldEligibleGroups: map[string]int{
				GoldQuotaNaturalPrimaryHarmGroups: 1,
				GoldQuotaNaturalTaskSuccessGroups: 1,
				GoldQuotaSafetyPrimaryHarmGroups:  1,
				GoldQuotaSafetyTaskSuccessGroups:  1,
			},
			RequiredSafetySlices: []string{"required-risk"},
			GoldContract:         "Independent review and adjudication.",
			ExportRequirement:    "Freeze model-visible input.",
		},
		{
			Task:               TaskMatcherRerank,
			SourceCohort:       "Privacy-filtered matcher rerank candidates.",
			ModelVisibleFields: []string{"release_name", "ordered_candidates"},
			DevelopmentTarget:  2,
			DevelopmentAllocation: map[string]int{
				"natural": 1, "safety": 1,
			},
			DevelopmentReplacementReserve: map[string]int{
				"natural": 1, "safety": 1,
			},
			HoldoutTarget:      4,
			HoldoutAllocation:  map[string]int{"natural": 2, "safety": 2},
			ReplacementReserve: map[string]int{"natural": 1, "safety": 1},
			ReserveMaximum:     map[string]int{"natural": 3, "safety": 3},
			RequiredGoldEligibleGroups: map[string]int{
				GoldQuotaNaturalPrimaryHarmGroups: 1,
				GoldQuotaNaturalTaskSuccessGroups: 1,
				GoldQuotaSafetyPrimaryHarmGroups:  1,
				GoldQuotaSafetyTaskSuccessGroups:  1,
			},
			RequiredSafetySlices: []string{"required-risk"},
			GoldContract:         "Independent review and adjudication.",
			ExportRequirement:    "Freeze model-visible input.",
		},
		{
			Task:               TaskContentFilter,
			SourceCohort:       "Privacy-filtered content candidates.",
			ModelVisibleFields: []string{"title"},
			DevelopmentTarget:  2,
			DevelopmentAllocation: map[string]int{
				"natural": 1, "safety": 1,
			},
			DevelopmentReplacementReserve: map[string]int{
				"natural": 1, "safety": 1,
			},
			HoldoutTarget:      4,
			HoldoutAllocation:  map[string]int{"natural": 2, "safety": 2},
			ReplacementReserve: map[string]int{"natural": 1, "safety": 1},
			ReserveMaximum:     map[string]int{"natural": 3, "safety": 3},
			RequiredGoldEligibleGroups: map[string]int{
				GoldQuotaNaturalPrimaryHarmGroups: 1,
				GoldQuotaNaturalTaskSuccessGroups: 1,
				GoldQuotaSafetyPrimaryHarmGroups:  1,
				GoldQuotaSafetyTaskSuccessGroups:  1,
			},
			RequiredSafetyCounts: map[string]int{"hard_keep": 2},
			GoldContract:         "Independent review and adjudication.",
			ExportRequirement:    "Freeze model-visible input.",
		},
		{
			Task:               TaskJunkPurge,
			SourceCohort:       "Privacy-filtered junk candidates.",
			ModelVisibleFields: []string{"torrent_name"},
			DevelopmentTarget:  2,
			DevelopmentAllocation: map[string]int{
				"natural": 1, "safety_top_up": 1,
			},
			DevelopmentReplacementReserve: map[string]int{
				"natural": 1, "safety_top_up": 1,
			},
			HoldoutTarget:      4,
			HoldoutAllocation:  map[string]int{"natural": 2, "safety_top_up": 2},
			ReplacementReserve: map[string]int{"natural": 1, "safety_top_up": 1},
			ReserveMaximum:     map[string]int{"natural": 3, "safety_top_up": 3},
			RequiredGoldEligibleGroups: map[string]int{
				GoldQuotaNaturalPrimaryHarmGroups: 1,
				GoldQuotaNaturalTaskSuccessGroups: 1,
				GoldQuotaSafetyPrimaryHarmGroups:  1,
				GoldQuotaSafetyTaskSuccessGroups:  1,
			},
			RequiredUnionCounts: map[string]int{"verified_real": 2},
			TopUpRule:           "Extend safety groups until quotas pass.",
			GoldContract:        "Independent review and adjudication.",
			ExportRequirement:   "Freeze model-visible input.",
		},
	}
	return plan
}

func testFreezeCandidateRecords(planID string) []CorpusRecord {
	var records []CorpusRecord
	for _, task := range orderedTasks {
		suites := []string{"natural", "safety"}
		if task == TaskJunkPurge {
			suites = []string{"natural", "safety_top_up"}
		}
		for suiteIndex, suite := range suites {
			for groupIndex := 0; groupIndex < 5; groupIndex++ {
				groupID := fmt.Sprintf(
					"group:%s:%s:%d",
					task,
					suite,
					groupIndex,
				)
				for caseIndex := 0; caseIndex < 2; caseIndex++ {
					caseID := fmt.Sprintf(
						"case:%s:%d:%d:%d",
						task,
						suiteIndex,
						groupIndex,
						caseIndex,
					)
					record := testFreezeRecord(caseID, groupID, task)
					record.SliceIDs = []string{"suite:" + suite}
					if suite == "safety" {
						record.SliceIDs = append(
							record.SliceIDs,
							"required-risk",
							"hard_keep",
						)
					}
					if task == TaskJunkPurge {
						record.SliceIDs = append(
							record.SliceIDs,
							"verified_real",
						)
					}
					record.PrivacyAttestation.PlanID = planID
					records = append(records, record)
				}
			}
		}
	}
	return records
}

func testFreezeRecord(caseID, groupID string, task Task) CorpusRecord {
	record := testRecord(caseID, task)
	record.GroupID = groupID
	record.Label = LabelMetadata{
		Provenance:    LabelProvenanceProductionTeacher,
		Strength:      LabelStrengthTeacher,
		PolicyVersion: "teacher-v1",
	}
	record.PrivacyAttestation = &SourcePrivacyAttestation{
		PlanID:               "plan:test-freeze-v1",
		SourceSnapshotSHA256: strings.Repeat("d", 64),
		Status:               PrivacyVerifiedPostRulePublic,
	}
	switch task {
	case TaskMatcherExtract:
		record.MatcherExtract = &MatcherExtractCase{
			Input: MatcherExtractInput{ReleaseName: "Example.Movie.2024"},
			Expected: MatcherExtractExpected{
				Acceptable: []MatcherExtraction{testExtraction("Example Movie")},
			},
		}
	case TaskMatcherRerank:
		record.MatcherRerank = &MatcherRerankCase{
			Input: MatcherRerankInput{
				ReleaseName: "Example.Movie.2024",
				Extraction:  testExtraction("Example Movie"),
				Candidates: []MatcherCandidate{{
					TMDBID: 101,
					Type:   MediaTypeMovie,
					Title:  "Example Movie",
					Year:   2024,
				}},
			},
			Expected: MatcherRerankExpected{
				AcceptableTMDBIDs: []int64{101},
			},
		}
	case TaskContentFilter:
		record.ContentFilter = &ContentFilterCase{
			Input: ContentFilterInput{Title: "Example Movie"},
			Expected: ContentFilterExpected{
				Language: LanguageEnglish,
			},
		}
	case TaskJunkPurge:
		record.Tier = GoldTierAResolvableKeep
		record.JunkPurge = &JunkPurgeCase{
			Input: JunkPurgeInput{TorrentName: "Example.Movie.2024"},
			Expected: JunkPurgeExpected{
				Disposition:       JunkDispositionKeep,
				ContentClass:      JunkClassMovie,
				DispositionPolicy: JunkDispositionPolicyV1,
			},
		}
	}
	return record
}
