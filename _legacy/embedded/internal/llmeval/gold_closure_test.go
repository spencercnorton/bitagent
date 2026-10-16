package llmeval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFinalizeGoldCorpusDeterministicallyReplacesUnresolvedGroupsAndClosesQuotas(
	t *testing.T,
) {
	input, unresolvedGroups := goldClosureFixture(t)

	first, err := FinalizeGoldCorpus(input)
	require.NoError(t, err)
	second, err := FinalizeGoldCorpus(input)
	require.NoError(t, err)
	require.Equal(t, first, second)

	require.Equal(t, GoldClosureStatus, first.Manifest.Status)
	require.Equal(t, GoldClosureAlgorithmID, first.Manifest.SelectionAlgorithmID)
	require.Equal(t, input.PlanSHA256, first.Manifest.PlanSHA256)
	require.Equal(
		t,
		input.CandidateFreezeManifestSHA256,
		first.Manifest.CandidateFreezeManifestSHA256,
	)
	require.Equal(
		t,
		input.CandidateFreezeManifest.PrivacySidecarSHA256,
		first.Manifest.PrivacySidecarSHA256,
	)
	require.Len(t, first.Development.Records, 8)
	require.Len(t, first.Holdout.Records, 16)
	require.Equal(t, first.Development.SHA256, first.Manifest.DevelopmentCorpusSHA256)
	require.Equal(t, first.Holdout.SHA256, first.Manifest.HoldoutCorpusSHA256)
	require.Len(t, first.Manifest.ReviewWorkflows, 2)
	require.Equal(
		t,
		ReviewWorkflowMatcher,
		first.Manifest.ReviewWorkflows[0].Workflow,
	)
	require.Equal(
		t,
		ReviewWorkflowContentJunk,
		first.Manifest.ReviewWorkflows[1].Workflow,
	)

	developmentGroups := make(map[string]struct{})
	for _, record := range first.Development.Records {
		require.Equal(t, LabelStrengthGold, record.Label.Strength)
		require.Equal(t, LabelProvenanceHumanReview, record.Label.Provenance)
		require.NotNil(t, record.Label.HumanReviewProof)
		require.NotNil(t, record.PrivacyAttestation)
		key := string(record.Task) + "\x00" + record.GroupID
		developmentGroups[key] = struct{}{}
		require.NotContains(t, unresolvedGroups, key)
	}
	for _, record := range first.Holdout.Records {
		key := string(record.Task) + "\x00" + record.GroupID
		_, overlaps := developmentGroups[key]
		require.False(t, overlaps)
		require.NotContains(t, unresolvedGroups, key)
	}

	for _, summary := range first.Manifest.Tasks {
		require.Equal(t, 2, summary.DevelopmentCases)
		require.Equal(t, 2, summary.DevelopmentGroups)
		require.Equal(t, 4, summary.HoldoutCases)
		require.Equal(t, 2, summary.HoldoutGroups)
		for _, allocation := range summary.HoldoutAllocations {
			require.GreaterOrEqual(t, allocation.ActualCases, allocation.MinimumCases)
		}
		for quota, count := range summary.RequiredGoldQuotaCounts {
			require.GreaterOrEqual(
				t,
				count,
				goldQuotaMinimum(input.Plan.Tasks[taskIndex(input.Plan, summary.Task)], quota),
			)
		}
	}
}

func TestFinalizeGoldCorpusFailsClosedOnTamperingAndGoldQuotaContradiction(
	t *testing.T,
) {
	input, _ := goldClosureFixture(t)

	tamperedManifest := input
	tamperedManifest.CandidateFreezeManifest.CandidateCases++
	_, err := FinalizeGoldCorpus(tamperedManifest)
	require.ErrorContains(t, err, "freeze manifest does not match")

	tamperedGold := input
	records := append(
		[]CorpusRecord(nil),
		tamperedGold.MatcherPromotion.GoldCorpus.Records...,
	)
	for i := range records {
		if records[i].Task == TaskMatcherExtract {
			payload := *records[i].MatcherExtract
			payload.Input.ReleaseName += ".tampered"
			records[i].MatcherExtract = &payload
			break
		}
	}
	changed, err := NewCorpus(records)
	require.NoError(t, err)
	tamperedGold.MatcherPromotion.GoldCorpus = &changed
	_, err = FinalizeGoldCorpus(tamperedGold)
	require.ErrorContains(t, err, "changed frozen input")

	missingHash := input
	missingHash.MatcherArtifacts.PrimarySubmissionBSHA256 = ""
	_, err = FinalizeGoldCorpus(missingHash)
	require.ErrorContains(t, err, "primary_submission_b_sha256")

	placeholder := input
	records = append(
		[]CorpusRecord(nil),
		placeholder.ContentJunkPromotion.GoldCorpus.Records...,
	)
	for i := range records {
		if records[i].Task != TaskContentFilter {
			continue
		}
		records[i].Label = LabelMetadata{
			Provenance:    LabelProvenanceSamplingCandidate,
			Strength:      LabelStrengthUnreviewed,
			PolicyVersion: CandidatePlaceholderPolicyVersion,
		}
		payload := *records[i].ContentFilter
		payload.Expected = ContentFilterExpected{AllowAbstain: true}
		records[i].ContentFilter = &payload
		break
	}
	changed, err = NewCorpus(records)
	require.NoError(t, err)
	placeholder.ContentJunkPromotion.GoldCorpus = &changed
	_, err = FinalizeGoldCorpus(placeholder)
	require.ErrorContains(t, err, "label is not bound")

	contradicted := input
	records = append(
		[]CorpusRecord(nil),
		contradicted.ContentJunkPromotion.GoldCorpus.Records...,
	)
	for i := range records {
		if records[i].Task != TaskContentFilter ||
			!containsString(records[i].SliceIDs, "hard_must_keep_english") {
			continue
		}
		payload := *records[i].ContentFilter
		payload.Expected = ContentFilterExpected{Language: LanguageNonEnglish}
		records[i].ContentFilter = &payload
	}
	changed, err = NewCorpus(records)
	require.NoError(t, err)
	contradicted.ContentJunkPromotion.GoldCorpus = &changed
	_, err = FinalizeGoldCorpus(contradicted)
	require.ErrorContains(t, err, "cannot satisfy reviewed-gold quotas")
}

func TestReadCorpusFreezeManifestIsStrictAndBindsExactBytes(t *testing.T) {
	input, _ := goldClosureFixture(t)
	raw, err := json.Marshal(input.CandidateFreezeManifest)
	require.NoError(t, err)

	manifest, sha, err := ReadCorpusFreezeManifest(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, input.CandidateFreezeManifest, manifest)
	require.Len(t, sha, 64)

	_, withLF, err := ReadCorpusFreezeManifest(
		bytes.NewReader(append(append([]byte(nil), raw...), '\n')),
	)
	require.NoError(t, err)
	require.NotEqual(t, sha, withLF)

	_, _, err = ReadCorpusFreezeManifest(strings.NewReader(
		`{"schema_version":1,"schema_version":1}`,
	))
	require.ErrorContains(t, err, "duplicate object key")

	var unknown map[string]any
	require.NoError(t, json.Unmarshal(raw, &unknown))
	unknown["unexpected"] = true
	unknownRaw, err := json.Marshal(unknown)
	require.NoError(t, err)
	_, _, err = ReadCorpusFreezeManifest(bytes.NewReader(unknownRaw))
	require.ErrorContains(t, err, "unknown field")
}

func TestGoldClosureManifestStrictlyBindsOnlyItsCompleteFinalCorpora(t *testing.T) {
	input, _ := goldClosureFixture(t)
	closure, err := FinalizeGoldCorpus(input)
	require.NoError(t, err)
	require.NoError(
		t,
		ValidateGoldClosureCorpus(closure.Development, closure.Manifest),
	)
	require.NoError(t, ValidateGoldClosureCorpus(closure.Holdout, closure.Manifest))

	prefix, err := NewCorpus(closure.Holdout.Records[:1])
	require.NoError(t, err)
	require.ErrorContains(
		t,
		ValidateGoldClosureCorpus(prefix, closure.Manifest),
		"neither final development nor holdout",
	)

	tampered := closure.Manifest
	tampered.ReviewWorkflows[0].PolicySHA256 = strings.Repeat("0", 64)
	require.ErrorContains(
		t,
		ValidateGoldClosureCorpus(closure.Holdout, tampered),
		"human-review proof does not match",
	)

	notExecutable := closure.Manifest
	notExecutable.PlanExecutable = false
	require.ErrorContains(
		t,
		ValidateGoldClosureCorpus(closure.Holdout, notExecutable),
		"plan_executable must be true",
	)

	exporterUnverified := closure.Manifest
	exporterUnverified.ProductionSourceExporterVerified = false
	require.ErrorContains(
		t,
		ValidateGoldClosureCorpus(closure.Holdout, exporterUnverified),
		"production_source_exporter_verified must be true",
	)

	missingSidecar := closure.Manifest
	missingSidecar.PrivacySidecarSHA256 = ""
	require.ErrorContains(
		t,
		ValidateGoldClosureCorpus(closure.Holdout, missingSidecar),
		"privacy_sidecar_sha256",
	)

	raw, err := json.Marshal(closure.Manifest)
	require.NoError(t, err)
	decoded, sha, err := ReadGoldClosureManifest(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, closure.Manifest, decoded)
	require.Len(t, sha, 64)
	_, _, err = ReadGoldClosureManifest(bytes.NewReader(append(raw, []byte(`{}`)...)))
	require.Error(t, err)
}

func TestGoldExecutionPrivacySidecarMustMatchClosureAndCoverSelectedCases(
	t *testing.T,
) {
	input, _ := goldClosureFixture(t)
	entries := make(
		[]ProductionPrivacySidecarEntry,
		0,
		len(input.Candidates.Records),
	)
	for i, record := range input.Candidates.Records {
		entries = append(entries, ProductionPrivacySidecarEntry{
			CaseID: record.CaseID,
			Task:   record.Task,
			InfoHashHex: strings.Repeat(
				fmt.Sprintf("%x", (i%15)+1),
				40,
			),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Task != entries[j].Task {
			return entries[i].Task < entries[j].Task
		}
		return entries[i].CaseID < entries[j].CaseID
	})
	sidecar := ProductionPrivacySidecar{
		SchemaVersion:         SchemaVersion,
		Status:                ProductionPrivacySidecarStatus,
		Scope:                 ProductionPrivacySidecarScope,
		PlanID:                input.Plan.PlanID,
		PlanSHA256:            input.PlanSHA256,
		CandidateCorpusSHA256: input.Candidates.SHA256,
		Entries:               entries,
	}
	raw, err := MarshalProductionPrivacySidecar(sidecar)
	require.NoError(t, err)
	sidecarSHA256 := sha256Hex(raw)
	input.CandidateFreezeManifest.PrivacySidecarSHA256 = sidecarSHA256
	closure, err := FinalizeGoldCorpus(input)
	require.NoError(t, err)
	require.NoError(t, ValidateGoldExecutionPrivacySidecar(
		closure.Holdout,
		closure.Manifest,
		sidecar,
		sidecarSHA256,
	))

	missing := sidecar
	missing.Entries = append(
		[]ProductionPrivacySidecarEntry(nil),
		sidecar.Entries...,
	)
	for i, entry := range missing.Entries {
		if entry.CaseID == closure.Holdout.Records[0].CaseID {
			missing.Entries = append(
				missing.Entries[:i],
				missing.Entries[i+1:]...,
			)
			break
		}
	}
	require.ErrorContains(t, ValidateGoldExecutionPrivacySidecar(
		closure.Holdout,
		closure.Manifest,
		missing,
		sidecarSHA256,
	), "cover every selected gold case")

	require.ErrorContains(t, ValidateGoldExecutionPrivacySidecar(
		closure.Holdout,
		closure.Manifest,
		sidecar,
		strings.Repeat("0", 64),
	), "do not match the gold closure manifest")
}

func TestPhaseSeparatedGoldClosureFreezesFinalistsBeforeHoldoutReview(
	t *testing.T,
) {
	input := phasedGoldClosureFixture(t)

	development, err := FinalizeDevelopmentGold(input.development)
	require.NoError(t, err)
	require.Len(t, development.Development.Records, 8)
	require.Equal(
		t,
		development.Development.SHA256,
		development.Manifest.DevelopmentCorpusSHA256,
	)

	developmentRaw, err := json.Marshal(development.Manifest)
	require.NoError(t, err)
	developmentSHA := sha256Hex(developmentRaw)
	normalizedConfig := promotionTestSystem(
		"stage1-normalized-control",
		"openai",
		1.0,
	)
	normalizedConfig.Tasks = append([]Task(nil), orderedTasks...)
	productionConfig := promotionTestSystem(
		"stage1-production-control",
		"openai",
		1.0,
	)
	productionConfig.EvaluationLane = EvaluationLaneProductionFidelity
	productionConfig.Tasks = append([]Task(nil), orderedTasks...)
	productionConfig.OutputContract = OutputContractPromptOnly
	productionConfig.StructuredOutputs = false
	candidateConfig := promotionTestSystem(
		"stage1-candidate",
		"openrouter",
		0.01,
	)
	candidateConfig.Tasks = append([]Task(nil), orderedTasks...)
	systemManifest := SystemManifest{
		SchemaVersion: SchemaVersion,
		Systems: []SystemConfig{
			normalizedConfig,
			productionConfig,
			candidateConfig,
		},
	}
	require.NoError(t, systemManifest.Validate())
	systemManifestRaw, err := json.Marshal(systemManifest)
	require.NoError(t, err)
	systemManifestSHA := sha256Hex(systemManifestRaw)
	finalists := FinalistRosterManifest{
		SchemaVersion:                    SchemaVersion,
		ProtocolVersion:                  FinalistRosterProtocolVersion,
		PlanSHA256:                       input.development.PlanSHA256,
		DevelopmentClosureManifestSHA256: developmentSHA,
		DevelopmentCorpusSHA256:          development.Development.SHA256,
		SystemManifestSHA256:             systemManifestSHA,
		Ordering:                         FinalistRosterOrdering,
	}
	for _, task := range orderedTasks {
		taskCorpus, err := FilterCorpus(development.Development, task, 0)
		require.NoError(t, err)
		roster := finalistTaskRosterForTest(
			t,
			development.Development,
			task,
			strings.Repeat("e", 64),
			strings.Repeat("f", 64),
		)
		roster = finalistTaskRosterBindManifestForTest(
			t,
			roster,
			taskCorpus,
			systemManifest,
		)
		finalists.Tasks = append(
			finalists.Tasks,
			roster,
		)
	}
	require.NoError(t, finalists.Validate(systemManifest, systemManifestSHA))
	finalistRaw, err := json.Marshal(finalists)
	require.NoError(t, err)
	finalistSHA := sha256Hex(finalistRaw)

	holdoutInput := GoldHoldoutClosureInput{
		Plan:                          input.development.Plan,
		PlanSHA256:                    input.development.PlanSHA256,
		Candidates:                    input.development.Candidates,
		CandidateFreezeManifest:       input.development.CandidateFreezeManifest,
		CandidateFreezeManifestSHA256: input.development.CandidateFreezeManifestSHA256,
		Development:                   development.Development,
		DevelopmentManifest:           development.Manifest,
		DevelopmentManifestSHA256:     developmentSHA,
		FinalistRoster:                finalists,
		FinalistRosterSHA256:          finalistSHA,
		SystemManifest:                systemManifest,
		SystemManifestSHA256:          systemManifestSHA,
		MatcherPromotion:              input.holdoutMatcher,
		MatcherArtifacts:              goldClosureTestArtifactHashes("2", false),
		ContentJunkPromotion:          input.holdoutContentJunk,
		ContentJunkArtifacts:          goldClosureTestArtifactHashes("3", false),
	}
	closure, err := FinalizeHoldoutGold(holdoutInput)
	require.NoError(t, err)
	require.True(t, closure.Manifest.ReviewPhaseSeparated)
	require.Equal(
		t,
		developmentSHA,
		closure.Manifest.DevelopmentClosureManifestSHA256,
	)
	require.Equal(
		t,
		finalistSHA,
		closure.Manifest.FinalistRosterManifestSHA256,
	)
	require.Equal(t, development.Development, closure.Development)
	require.Len(t, closure.Holdout.Records, 16)

	tampered := holdoutInput
	tampered.FinalistRoster.DevelopmentClosureManifestSHA256 =
		strings.Repeat("0", 64)
	_, err = FinalizeHoldoutGold(tampered)
	require.ErrorContains(
		t,
		err,
		"does not bind the exact completed development phase",
	)

	wrongManifestBinding := holdoutInput
	wrongManifestBinding.SystemManifestSHA256 = strings.Repeat("0", 64)
	_, err = FinalizeHoldoutGold(wrongManifestBinding)
	require.ErrorContains(
		t,
		err,
		"does not match the exact system manifest",
	)

	tamperedCost := holdoutInput
	tamperedRosterRaw, err := json.Marshal(holdoutInput.FinalistRoster)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(
		tamperedRosterRaw,
		&tamperedCost.FinalistRoster,
	))
	costTampered := false
	for taskIndex := range tamperedCost.FinalistRoster.Tasks {
		for evidenceIndex := range tamperedCost.FinalistRoster.
			Tasks[taskIndex].SystemEvidence {
			evidence := &tamperedCost.FinalistRoster.
				Tasks[taskIndex].SystemEvidence[evidenceIndex]
			if evidence.Role != PromotionRoleCandidate {
				continue
			}
			evidence.ManifestRecomputedStageOneCostMicroUSD++
			costTampered = true
			break
		}
		if costTampered {
			break
		}
	}
	require.True(t, costTampered)
	_, err = FinalizeHoldoutGold(tamperedCost)
	require.ErrorContains(
		t,
		err,
		"manifest-recomputed Stage 1 cost",
	)

	unbound := input.development
	unboundRecords := append(
		[]CorpusRecord(nil),
		unbound.Candidates.Records...,
	)
	for index := range unboundRecords {
		filtered := make(
			[]string,
			0,
			len(unboundRecords[index].SliceIDs),
		)
		for _, sliceID := range unboundRecords[index].SliceIDs {
			if strings.HasPrefix(
				sliceID,
				CorpusReviewPhaseSlicePrefix,
			) {
				continue
			}
			filtered = append(filtered, sliceID)
		}
		unboundRecords[index].SliceIDs = filtered
	}
	unbound.Candidates, err = NewCorpus(unboundRecords)
	require.NoError(t, err)
	_, err = FinalizeDevelopmentGold(unbound)
	require.ErrorContains(t, err, "is not phase-bound before review")
}

type phasedGoldClosureTestInput struct {
	development        GoldDevelopmentClosureInput
	holdoutMatcher     MatcherGoldPromotion
	holdoutContentJunk GoldPromotion
}

func phasedGoldClosureFixture(t *testing.T) phasedGoldClosureTestInput {
	t.Helper()
	plan := testCorpusPlan()
	plan.Executable = true
	plan.NonExecutableReason = ""
	plan.Readiness.PostReviewReplacementWorkflow = true
	plan.Readiness.ProductionSourceExporter = true
	plan.Tasks[2].RequiredSafetyCounts = map[string]int{
		"hard_must_keep_english": 2,
	}
	plan.Tasks[3].RequiredUnionCounts = map[string]int{
		"human_verified_real_movie_or_tv": 2,
	}
	require.NoError(t, plan.Validate())

	var records []CorpusRecord
	for _, record := range testFreezeCandidateRecords(plan.PlanID) {
		if !strings.HasSuffix(record.CaseID, ":0") {
			continue
		}
		separator := strings.LastIndex(record.GroupID, ":")
		require.NotEqual(t, -1, separator)
		groupIndex, err := strconv.Atoi(record.GroupID[separator+1:])
		require.NoError(t, err)
		phase := CorpusReviewPhaseHoldout
		if groupIndex < 2 {
			phase = CorpusReviewPhaseDevelopment
		}
		record.SliceIDs = append(
			record.SliceIDs,
			CorpusReviewPhaseSlicePrefix+phase,
		)
		switch record.Task {
		case TaskContentFilter:
			record.SliceIDs = replaceSlice(
				record.SliceIDs,
				"hard_keep",
				"hard_must_keep_english",
			)
		case TaskJunkPurge:
			record.SliceIDs = replaceSlice(
				record.SliceIDs,
				"verified_real",
				"human_verified_real_movie_or_tv",
			)
		}
		records = append(records, record)
	}
	candidates, err := NewCorpus(records)
	require.NoError(t, err)
	planRaw, err := json.Marshal(plan)
	require.NoError(t, err)
	_, planSHA, err := ReadCorpusPlan(bytes.NewReader(planRaw))
	require.NoError(t, err)
	frozen, err := FreezeCandidateCorpus(plan, planSHA, candidates)
	require.NoError(t, err)
	frozen.Manifest.Status = ProductionCorpusFreezeStatus
	frozen.Manifest.Scope = ProductionCorpusFreezeScope
	frozen.Manifest.ProductionSourceExporterVerified = true
	frozen.Manifest.ProductionExportManifestSHA256 =
		strings.Repeat("6", 64)
	frozen.Manifest.PrivacySidecarSHA256 = strings.Repeat("7", 64)

	return phasedGoldClosureTestInput{
		development: GoldDevelopmentClosureInput{
			Plan:                          plan,
			PlanSHA256:                    planSHA,
			Candidates:                    candidates,
			CandidateFreezeManifest:       frozen.Manifest,
			CandidateFreezeManifestSHA256: strings.Repeat("8", 64),
			MatcherPromotion: goldClosureMatcherPromotion(
				t,
				frozen.Development,
				"",
			),
			MatcherArtifacts: goldClosureTestArtifactHashes("a", false),
			ContentJunkPromotion: goldClosureContentJunkPromotion(
				t,
				frozen.Development,
				"",
			),
			ContentJunkArtifacts: goldClosureTestArtifactHashes("1", false),
		},
		holdoutMatcher: goldClosureMatcherPromotion(
			t,
			frozen.Holdout,
			"",
		),
		holdoutContentJunk: goldClosureContentJunkPromotion(
			t,
			frozen.Holdout,
			"",
		),
	}
}

func goldClosureFixture(
	t *testing.T,
) (GoldClosureInput, map[string]struct{}) {
	t.Helper()
	plan := testCorpusPlan()
	plan.Executable = true
	plan.NonExecutableReason = ""
	plan.Readiness.PostReviewReplacementWorkflow = true
	plan.Readiness.ProductionSourceExporter = true
	plan.Tasks[2].RequiredSafetyCounts = map[string]int{
		"hard_must_keep_english": 2,
	}
	plan.Tasks[3].RequiredUnionCounts = map[string]int{
		"human_verified_real_movie_or_tv": 2,
	}
	require.NoError(t, plan.Validate())

	records := testFreezeCandidateRecords(plan.PlanID)
	for i := range records {
		switch records[i].Task {
		case TaskContentFilter:
			records[i].SliceIDs = replaceSlice(
				records[i].SliceIDs,
				"hard_keep",
				"hard_must_keep_english",
			)
		case TaskJunkPurge:
			records[i].SliceIDs = replaceSlice(
				records[i].SliceIDs,
				"verified_real",
				"human_verified_real_movie_or_tv",
			)
		}
	}
	candidates, err := NewCorpus(records)
	require.NoError(t, err)
	planRaw, err := json.Marshal(plan)
	require.NoError(t, err)
	_, planSHA, err := ReadCorpusPlan(bytes.NewReader(planRaw))
	require.NoError(t, err)
	freeze, err := FreezeCandidateCorpus(plan, planSHA, candidates)
	require.NoError(t, err)
	freeze.Manifest.Status = ProductionCorpusFreezeStatus
	freeze.Manifest.Scope = ProductionCorpusFreezeScope
	freeze.Manifest.ProductionSourceExporterVerified = true
	freeze.Manifest.ProductionExportManifestSHA256 = strings.Repeat("e", 64)
	freeze.Manifest.PrivacySidecarSHA256 = strings.Repeat("7", 64)

	matcherAmbiguousID := ""
	contentAmbiguousID := ""
	unresolvedGroups := make(map[string]struct{})
	for _, record := range freeze.Development.Records {
		switch {
		case matcherAmbiguousID == "" && record.Task == TaskMatcherExtract:
			matcherAmbiguousID = record.CaseID
			unresolvedGroups[string(record.Task)+"\x00"+record.GroupID] = struct{}{}
		case contentAmbiguousID == "" && record.Task == TaskContentFilter:
			contentAmbiguousID = record.CaseID
			unresolvedGroups[string(record.Task)+"\x00"+record.GroupID] = struct{}{}
		}
	}
	require.NotEmpty(t, matcherAmbiguousID)
	require.NotEmpty(t, contentAmbiguousID)

	matcherPromotion := goldClosureMatcherPromotion(
		t,
		candidates,
		matcherAmbiguousID,
	)
	contentJunkPromotion := goldClosureContentJunkPromotion(
		t,
		candidates,
		contentAmbiguousID,
	)
	return GoldClosureInput{
		Plan:                          plan,
		PlanSHA256:                    planSHA,
		Candidates:                    candidates,
		CandidateFreezeManifest:       freeze.Manifest,
		CandidateFreezeManifestSHA256: strings.Repeat("9", 64),
		MatcherPromotion:              matcherPromotion,
		MatcherArtifacts: goldClosureTestArtifactHashes(
			"a",
			true,
		),
		ContentJunkPromotion: contentJunkPromotion,
		ContentJunkArtifacts: goldClosureTestArtifactHashes(
			"1",
			true,
		),
	}, unresolvedGroups
}

func goldClosureMatcherPromotion(
	t *testing.T,
	corpus Corpus,
	ambiguousID string,
) MatcherGoldPromotion {
	t.Helper()
	proof := testReviewProof(
		corpus,
		ReviewWorkflowMatcher,
		"matcher-policy-v1",
	)
	proof.SourceSnapshotSHA256 = strings.Repeat("d", 64)
	spec := MatcherReviewAssignmentSpec{
		ReviewSetID:   "review:matcher-gold-closure",
		PolicyVersion: proof.PolicyVersion,
		ReviewProof:   proof,
		Tasks:         []Task{TaskMatcherExtract, TaskMatcherRerank},
	}
	spec.ReviewerID = "reviewer:matcher-a"
	assignmentA, err := NewMatcherReviewAssignment(corpus, spec)
	require.NoError(t, err)
	spec.ReviewerID = "reviewer:matcher-b"
	assignmentB, err := NewMatcherReviewAssignment(corpus, spec)
	require.NoError(t, err)

	decisions := make(map[string]MatcherReviewDecision)
	for _, record := range corpus.Records {
		switch record.Task {
		case TaskMatcherExtract:
			decisions[record.CaseID] = MatcherReviewDecision{
				MatcherExtract: &MatcherExtractReviewDecision{
					Acceptable: append(
						[]MatcherExtraction(nil),
						record.MatcherExtract.Expected.Acceptable...,
					),
					AllowAbstain: record.MatcherExtract.Expected.AllowAbstain,
				},
			}
		case TaskMatcherRerank:
			decisions[record.CaseID] = MatcherReviewDecision{
				MatcherRerank: &MatcherRerankReviewDecision{
					AcceptableTMDBIDs: append(
						[]int64(nil),
						record.MatcherRerank.Expected.AcceptableTMDBIDs...,
					),
					AllowAbstain: record.MatcherRerank.Expected.AllowAbstain,
				},
			}
		}
	}
	if ambiguousID != "" {
		ambiguous := decisions[ambiguousID]
		ambiguous.MatcherExtract = &MatcherExtractReviewDecision{Ambiguous: true}
		decisions[ambiguousID] = ambiguous
	}
	submissionA := mustMatcherReviewSubmission(t, corpus, assignmentA, decisions)
	submissionB := mustMatcherReviewSubmission(t, corpus, assignmentB, decisions)
	if ambiguousID == "" {
		promotion, err := PromoteMatcherGold(
			corpus,
			submissionA,
			submissionB,
			nil,
		)
		require.NoError(t, err)
		return promotion
	}
	adjudicationAssignment, _, err := NewMatcherAdjudicationAssignment(
		corpus,
		"reviewer:matcher-adjudicator",
		submissionA,
		submissionB,
	)
	require.NoError(t, err)
	adjudication := mustMatcherReviewSubmission(
		t,
		corpus,
		adjudicationAssignment,
		map[string]MatcherReviewDecision{
			ambiguousID: {
				MatcherExtract: &MatcherExtractReviewDecision{Ambiguous: true},
			},
		},
	)
	promotion, err := PromoteMatcherGold(
		corpus,
		submissionA,
		submissionB,
		&adjudication,
	)
	require.NoError(t, err)
	return promotion
}

func goldClosureContentJunkPromotion(
	t *testing.T,
	corpus Corpus,
	ambiguousID string,
) GoldPromotion {
	t.Helper()
	proof := testReviewProof(
		corpus,
		ReviewWorkflowContentJunk,
		"content-junk-policy-v1",
	)
	proof.SourceSnapshotSHA256 = strings.Repeat("d", 64)
	spec := ReviewAssignmentSpec{
		ReviewSetID:   "review:content-junk-gold-closure",
		PolicyVersion: proof.PolicyVersion,
		ReviewProof:   proof,
		Tasks:         []Task{TaskContentFilter, TaskJunkPurge},
	}
	spec.ReviewerID = "reviewer:content-junk-a"
	assignmentA, err := NewReviewAssignment(corpus, spec)
	require.NoError(t, err)
	spec.ReviewerID = "reviewer:content-junk-b"
	assignmentB, err := NewReviewAssignment(corpus, spec)
	require.NoError(t, err)

	labels := make(map[string]ReviewLabel)
	for _, record := range corpus.Records {
		switch record.Task {
		case TaskContentFilter:
			labels[record.CaseID] = ReviewLabelEnglish
		case TaskJunkPurge:
			labels[record.CaseID] = ReviewLabel(JunkClassMovie)
		}
	}
	if ambiguousID != "" {
		labels[ambiguousID] = ReviewLabelAmbiguous
	}
	submissionA := mustReviewSubmission(t, corpus, assignmentA, labels)
	submissionB := mustReviewSubmission(t, corpus, assignmentB, labels)
	if ambiguousID == "" {
		promotion, err := PromoteGoldLabels(
			corpus,
			submissionA,
			submissionB,
			nil,
		)
		require.NoError(t, err)
		return promotion
	}
	adjudicationAssignment, _, err := NewAdjudicationAssignment(
		corpus,
		"reviewer:content-junk-adjudicator",
		submissionA,
		submissionB,
	)
	require.NoError(t, err)
	adjudication := mustReviewSubmission(
		t,
		corpus,
		adjudicationAssignment,
		map[string]ReviewLabel{ambiguousID: ReviewLabelAmbiguous},
	)
	promotion, err := PromoteGoldLabels(
		corpus,
		submissionA,
		submissionB,
		&adjudication,
	)
	require.NoError(t, err)
	return promotion
}

func goldClosureTestArtifactHashes(
	start string,
	adjudicated bool,
) GoldClosureReviewArtifactHashes {
	values := []string{start, "b", "c", "e", "f", "8"}
	hashes := GoldClosureReviewArtifactHashes{
		PrimaryAssignmentASHA256: strings.Repeat(values[0], 64),
		PrimarySubmissionASHA256: strings.Repeat(values[1], 64),
		PrimaryAssignmentBSHA256: strings.Repeat(values[2], 64),
		PrimarySubmissionBSHA256: strings.Repeat(values[3], 64),
	}
	if adjudicated {
		hashes.AdjudicationAssignmentSHA256 = strings.Repeat(values[4], 64)
		hashes.AdjudicationSubmissionSHA256 = strings.Repeat(values[5], 64)
	}
	return hashes
}

func replaceSlice(values []string, old, replacement string) []string {
	out := append([]string(nil), values...)
	for i := range out {
		if out[i] == old {
			out[i] = replacement
		}
	}
	return out
}

func taskIndex(plan CorpusPlan, task Task) int {
	for i, candidate := range plan.Tasks {
		if candidate.Task == task {
			return i
		}
	}
	return -1
}

func TestGoldClosureInputHasNoModelResultSurface(t *testing.T) {
	typ := reflect.TypeOf(GoldClosureInput{})
	for i := 0; i < typ.NumField(); i++ {
		require.NotEqual(t, reflect.TypeOf(ResultRecord{}), typ.Field(i).Type)
		require.NotEqual(t, reflect.TypeOf([]ResultRecord{}), typ.Field(i).Type)
	}
}
