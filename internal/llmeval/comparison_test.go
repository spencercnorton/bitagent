package llmeval

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestExactMcNemarTwoSided(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		candidateOnly int
		controlOnly   int
		want          float64
	}{
		{name: "no discordance", want: 1},
		{name: "ten to zero", candidateOnly: 10, want: 0.001953125},
		{name: "three to seven", candidateOnly: 3, controlOnly: 7, want: 0.34375},
		{name: "balanced", candidateOnly: 5, controlOnly: 5, want: 1},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := ExactMcNemarTwoSided(test.candidateOnly, test.controlOnly)
			if math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("ExactMcNemarTwoSided(%d,%d) = %.15g, want %.15g",
					test.candidateOnly,
					test.controlOnly,
					got,
					test.want,
				)
			}
		})
	}
}

func TestCompareCandidateBetterPassesAllGates(t *testing.T) {
	records := comparisonContentRecords(100, true)
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(corpus, comparisonControlSystem(), func(index int) bool {
		return index >= 50
	})
	candidate := comparisonContentResults(corpus, comparisonCandidateSystem(), func(int) bool {
		return true
	})

	report, err := Compare(corpus, control, candidate, comparisonTestOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	task := onlyTaskComparison(t, report)
	if task.DiagnosticVerdict != ComparisonPass ||
		report.DiagnosticVerdict != ComparisonPass {
		t.Fatalf(
			"diagnostic verdict task=%q report=%q; gates=%+v",
			task.DiagnosticVerdict,
			report.DiagnosticVerdict,
			task.Gates,
		)
	}
	if report.Promotion.Eligible ||
		report.Promotion.Decision != ComparisonInconclusive {
		t.Fatalf(
			"diagnostic verdict task=%q report=%q eligibility=%+v",
			task.DiagnosticVerdict,
			report.DiagnosticVerdict,
			report.Promotion,
		)
	}
	if task.PrimaryHarm.Control.Count != 50 || task.PrimaryHarm.Candidate.Count != 0 {
		t.Fatalf("primary harm = %+v", task.PrimaryHarm)
	}
	if task.PrimaryHarm.CandidateMinusControl != -0.5 {
		t.Fatalf("harm difference = %v, want -0.5", task.PrimaryHarm.CandidateMinusControl)
	}
	if task.TaskSuccess.CandidateMinusControl != 0.5 {
		t.Fatalf("success difference = %v, want 0.5", task.TaskSuccess.CandidateMinusControl)
	}
	if task.PrimaryHarm.Discordance.ControlOnlyPositive != 50 ||
		task.PrimaryHarm.Discordance.CandidateOnlyPositive != 0 {
		t.Fatalf("harm discordance = %+v", task.PrimaryHarm.Discordance)
	}
	if task.Gates.ZeroSafetyHarm.Decision != ComparisonPass {
		t.Fatalf("safety gate = %+v", task.Gates.ZeroSafetyHarm)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if strings.Contains(string(encoded), `"verdict":`) ||
		strings.Contains(string(encoded), `"verdict_scope":`) {
		t.Fatalf("report retains ambiguous legacy verdict fields: %s", encoded)
	}
}

func TestCompareCandidateWorseFails(t *testing.T) {
	records := comparisonContentRecords(400, true)
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(corpus, comparisonControlSystem(), func(int) bool {
		return true
	})
	candidate := comparisonContentResults(corpus, comparisonCandidateSystem(), func(index int) bool {
		return index >= 2
	})

	report, err := Compare(corpus, control, candidate, comparisonTestOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	task := onlyTaskComparison(t, report)
	if task.DiagnosticVerdict != ComparisonFail ||
		report.DiagnosticVerdict != ComparisonFail {
		t.Fatalf(
			"diagnostic verdict task=%q report=%q; gates=%+v",
			task.DiagnosticVerdict,
			report.DiagnosticVerdict,
			task.Gates,
		)
	}
	if math.Abs(task.PrimaryHarm.CandidateMinusControl-0.005) > 1e-12 {
		t.Fatalf("harm difference = %v, want 0.005", task.PrimaryHarm.CandidateMinusControl)
	}
	if task.Gates.HarmNonInferiority.Decision != ComparisonFail ||
		task.Gates.HarmNonInferiority.ReasonCode != "point_estimate_outside_margin" {
		t.Fatalf("harm gate = %+v", task.Gates.HarmNonInferiority)
	}
	if task.Gates.ZeroSafetyHarm.Decision != ComparisonFail {
		t.Fatalf("safety gate = %+v", task.Gates.ZeroSafetyHarm)
	}
}

func TestCompareIdenticalSmallSampleIsInconclusive(t *testing.T) {
	records := comparisonContentRecords(20, true)
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(corpus, comparisonControlSystem(), func(int) bool {
		return true
	})
	candidate := comparisonContentResults(corpus, comparisonCandidateSystem(), func(int) bool {
		return true
	})

	report, err := Compare(corpus, control, candidate, comparisonTestOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	task := onlyTaskComparison(t, report)
	if task.DiagnosticVerdict != ComparisonInconclusive ||
		report.DiagnosticVerdict != ComparisonInconclusive {
		t.Fatalf(
			"diagnostic verdict task=%q report=%q; gates=%+v",
			task.DiagnosticVerdict,
			report.DiagnosticVerdict,
			task.Gates,
		)
	}
	if task.Gates.HarmNonInferiority.ReasonCode != "zero_discordance_underpowered" ||
		task.Gates.HarmNonInferiority.RequiredGroups != 1197 {
		t.Fatalf("harm gate = %+v", task.Gates.HarmNonInferiority)
	}
	if task.Gates.SuccessNonInferiority.ReasonCode != "zero_discordance_underpowered" ||
		task.Gates.SuccessNonInferiority.RequiredGroups != 299 {
		t.Fatalf("success gate = %+v", task.Gates.SuccessNonInferiority)
	}
}

func TestCompareTinyDiscordantSampleIsInconclusive(t *testing.T) {
	records := comparisonContentRecords(2, true)
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(corpus, comparisonControlSystem(), func(int) bool {
		return false
	})
	candidate := comparisonContentResults(corpus, comparisonCandidateSystem(), func(int) bool {
		return true
	})

	report, err := Compare(corpus, control, candidate, comparisonTestOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	task := onlyTaskComparison(t, report)
	if task.DiagnosticVerdict != ComparisonInconclusive ||
		report.DiagnosticVerdict != ComparisonInconclusive {
		t.Fatalf("diagnostic verdict task=%q report=%q; gates=%+v",
			task.DiagnosticVerdict,
			report.DiagnosticVerdict,
			task.Gates,
		)
	}
	for name, gate := range map[string]ComparisonGate{
		"harm":    task.Gates.HarmNonInferiority,
		"success": task.Gates.SuccessNonInferiority,
	} {
		if gate.Decision != ComparisonInconclusive ||
			gate.ReasonCode != "insufficient_independent_groups" ||
			gate.RequiredGroups != minimumBootstrapGroups {
			t.Fatalf("%s gate = %+v", name, gate)
		}
	}
}

func TestGroupedCancellationUsesConservativeZeroEffectPowerBound(t *testing.T) {
	observations := make([]pairedObservation, 0, 60)
	for group := 0; group < minimumBootstrapGroups; group++ {
		groupID := fmt.Sprintf("group:%02d", group)
		observations = append(
			observations,
			pairedObservation{
				groupID:   groupID,
				control:   true,
				candidate: false,
			},
			pairedObservation{
				groupID:   groupID,
				control:   false,
				candidate: true,
			},
		)
	}
	metric := summarizePairedIndicator(
		"primary_harm",
		"test cancellation",
		observations,
		comparisonTestOptions(),
		"grouped-cancellation-regression",
	)
	if metric.Discordance.Total != 60 ||
		metric.NonzeroEffectGroups != 0 ||
		metric.GroupClusterBootstrap95.Lower != 0 ||
		metric.GroupClusterBootstrap95.Upper != 0 {
		t.Fatalf("group-cancelled metric = %+v", metric)
	}
	gate := harmGate(metric)
	if gate.Decision != ComparisonInconclusive ||
		gate.ReasonCode != "zero_group_effect_underpowered" ||
		gate.RequiredGroups !=
			minimumZeroDiscordanceGroups(ComparisonHarmMargin) {
		t.Fatalf("group-cancelled harm gate falsely passed: %+v", gate)
	}
}

func TestCompareSchemaValidityGateCountsNonOKAsInvalid(t *testing.T) {
	records := comparisonContentRecords(100, true)
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(corpus, comparisonControlSystem(), func(int) bool {
		return true
	})
	candidate := comparisonContentResults(corpus, comparisonCandidateSystem(), func(int) bool {
		return true
	})
	candidate[0].Status = ResultStatusSchemaError
	candidate[0].ErrorCode = "invalid-json"
	candidate[0].ContentFilter = nil

	report, err := Compare(corpus, control, candidate, comparisonTestOptions())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	task := onlyTaskComparison(t, report)
	if task.SchemaValidity.Candidate.Count != 99 ||
		task.SchemaValidity.Candidate.Denominator != 100 {
		t.Fatalf("schema validity = %+v", task.SchemaValidity)
	}
	if task.Gates.SchemaValidity.Decision != ComparisonFail ||
		task.Gates.SchemaValidity.ReasonCode != "below_threshold" {
		t.Fatalf("schema gate = %+v", task.Gates.SchemaValidity)
	}
}

func TestCompareProductionBindingNeverBlendsNaturalAndSafetySuites(t *testing.T) {
	records := comparisonContentRecords(2, true)
	records[0].SliceIDs = []string{"suite:natural", "hard_must_keep_english"}
	records[1].SliceIDs = []string{"suite:safety", "hard_must_keep_english"}
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(
		corpus,
		comparisonControlSystem(),
		func(int) bool { return true },
	)
	candidate := comparisonContentResults(
		corpus,
		comparisonCandidateSystem(),
		func(index int) bool { return index == 0 },
	)
	options := comparisonTestOptions()
	options.ProductionSuiteBinding = true

	report, err := Compare(corpus, control, candidate, options)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	task := onlyTaskComparison(t, report)
	if task.PrimaryQualitySuite != "natural" ||
		len(task.Suites) != 2 ||
		task.PrimaryHarm.EligibleCases != 1 ||
		task.PrimaryHarm.Candidate.Count != 0 {
		t.Fatalf("primary natural comparison = %+v", task)
	}
	if len(task.SafetySuites) != 1 ||
		task.SafetySuites[0] != "safety" ||
		task.SafetyHarm.Candidate.Denominator != 1 ||
		task.SafetyHarm.Candidate.Count != 1 ||
		task.Gates.ZeroSafetyHarm.Decision != ComparisonFail {
		t.Fatalf("safety comparison = %+v", task)
	}
}

func TestCompareProductionSafetyNonActionCannotClearZeroHarm(t *testing.T) {
	tests := []struct {
		name           string
		mutateSafety   func(*ResultRecord)
		wantSchemaGate ComparisonDecision
	}{
		{
			name: "all runtime errors",
			mutateSafety: func(result *ResultRecord) {
				result.Status = ResultStatusError
				result.ErrorCode = "timeout"
				result.ContentFilter = nil
			},
			wantSchemaGate: ComparisonFail,
		},
		{
			name: "all wrong abstentions",
			mutateSafety: func(result *ResultRecord) {
				result.ContentFilter = &ContentFilterResult{
					Action: ContentFilterActionAbstain,
				}
			},
			wantSchemaGate: ComparisonPass,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			records := productionComparisonContentRecords(4, 4, false)
			corpus := mustCorpus(t, records)
			control := comparisonContentResults(
				corpus,
				comparisonControlSystem(),
				func(int) bool { return true },
			)
			candidate := comparisonContentResults(
				corpus,
				comparisonCandidateSystem(),
				func(int) bool { return true },
			)
			for index, record := range corpus.Records {
				if hasSlice(record.SliceIDs, "suite:safety") {
					test.mutateSafety(&candidate[index])
				}
			}
			options := comparisonTestOptions()
			options.ProductionSuiteBinding = true

			report, err := Compare(corpus, control, candidate, options)
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			task := onlyTaskComparison(t, report)
			if task.Gates.ZeroSafetyHarm.Decision != ComparisonPass ||
				task.SafetyHarm.Candidate.Count != 0 {
				t.Fatalf(
					"fixture unexpectedly had destructive harm: %+v",
					task.SafetyHarm,
				)
			}
			if task.Gates.SafetySuccessNonInferiority.Decision !=
				ComparisonFail ||
				task.Gates.SafetySuccessNonInferiority.ReasonCode !=
					"point_estimate_outside_margin" {
				t.Fatalf(
					"safety success gate = %+v",
					task.Gates.SafetySuccessNonInferiority,
				)
			}
			if task.Gates.SafetySchemaValidity.Decision !=
				test.wantSchemaGate {
				t.Fatalf(
					"safety schema gate = %+v",
					task.Gates.SafetySchemaValidity,
				)
			}
			if task.DiagnosticVerdict != ComparisonFail ||
				report.DiagnosticVerdict != ComparisonFail ||
				report.Promotion.Decision != ComparisonFail {
				t.Fatalf(
					"non-action safety suite passed: task=%q report=%q promotion=%q",
					task.DiagnosticVerdict,
					report.DiagnosticVerdict,
					report.Promotion.Decision,
				)
			}
		})
	}
}

func TestCompareProductionSafetyCountsGoldExpectedAbstentionAsSuccess(
	t *testing.T,
) {
	groups := minimumZeroDiscordanceGroups(ComparisonSuccessMargin)
	records := productionComparisonContentRecords(groups, groups, true)
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(
		corpus,
		comparisonControlSystem(),
		func(int) bool { return true },
	)
	candidate := comparisonContentResults(
		corpus,
		comparisonCandidateSystem(),
		func(int) bool { return true },
	)
	for index, record := range corpus.Records {
		if !hasSlice(record.SliceIDs, "suite:safety") {
			continue
		}
		control[index].ContentFilter = &ContentFilterResult{
			Action: ContentFilterActionAbstain,
		}
		candidate[index].ContentFilter = &ContentFilterResult{
			Action: ContentFilterActionAbstain,
		}
	}
	options := comparisonTestOptions()
	options.ProductionSuiteBinding = true

	report, err := Compare(corpus, control, candidate, options)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	task := onlyTaskComparison(t, report)
	if task.SafetyTaskSuccess.Candidate.Count != groups ||
		task.SafetyTaskSuccess.Candidate.Denominator != groups ||
		task.Gates.SafetySuccessNonInferiority.Decision != ComparisonPass ||
		task.Gates.SafetySchemaValidity.Decision != ComparisonPass {
		t.Fatalf(
			"gold abstention was not safety success: metric=%+v success_gate=%+v schema_gate=%+v",
			task.SafetyTaskSuccess,
			task.Gates.SafetySuccessNonInferiority,
			task.Gates.SafetySchemaValidity,
		)
	}
}

func TestCompareStrictPromotionRecognizesGoldHoldoutButFailsClosedOnProtocol(t *testing.T) {
	minimumGroups := minimumZeroDiscordanceGroups(ComparisonHarmMargin)
	records := comparisonContentRecords(minimumGroups, true)
	for i := range records {
		records[i].SliceIDs = append(records[i].SliceIDs, defaultHoldoutSliceID)
	}
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(corpus, comparisonControlSystem(), func(int) bool {
		return true
	})
	candidate := comparisonContentResults(corpus, comparisonCandidateSystem(), func(int) bool {
		return true
	})
	options := comparisonTestOptions()
	options.PromotionMode = true

	report, err := Compare(corpus, control, candidate, options)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if report.DiagnosticVerdict != ComparisonPass ||
		!report.Promotion.Requested ||
		!report.Promotion.CorpusEligible ||
		report.Promotion.ProtocolComplete ||
		report.Promotion.Eligible ||
		report.Promotion.Decision != ComparisonInconclusive ||
		!containsComparisonReason(
			report.Promotion.ReasonCodes,
			"required_protocol_artifacts_unavailable",
		) {
		t.Fatalf("promotion report = %+v", report)
	}
	if report.Promotion.MinimumIndependentGroups != minimumGroups ||
		report.Promotion.GoldCases != minimumGroups ||
		report.Promotion.IndependentlyReviewedGoldCases != minimumGroups ||
		report.Promotion.HoldoutCases != minimumGroups {
		t.Fatalf("promotion evidence = %+v", report.Promotion)
	}
	if len(report.Promotion.Tasks) != 1 ||
		!report.Promotion.Tasks[0].MeetsMinimum ||
		report.Promotion.Tasks[0].SafetyUniqueGroups != minimumGroups {
		t.Fatalf("promotion task evidence = %+v", report.Promotion.Tasks)
	}
}

func TestCompareStrictPromotionRejectsTinyGoldCorpus(t *testing.T) {
	records := comparisonContentRecords(100, true)
	for i := range records {
		records[i].SliceIDs = append(records[i].SliceIDs, defaultHoldoutSliceID)
	}
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(corpus, comparisonControlSystem(), func(index int) bool {
		return index >= 50
	})
	candidate := comparisonContentResults(corpus, comparisonCandidateSystem(), func(int) bool {
		return true
	})
	options := comparisonTestOptions()
	options.PromotionMode = true

	report, err := Compare(corpus, control, candidate, options)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if report.DiagnosticVerdict != ComparisonPass ||
		report.Promotion.Decision != ComparisonInconclusive ||
		report.Promotion.Eligible ||
		!containsComparisonReason(report.Promotion.ReasonCodes, "insufficient_task_groups") ||
		!containsComparisonReason(report.Promotion.ReasonCodes, "insufficient_safety_groups") {
		t.Fatalf("tiny-corpus promotion report = %+v", report)
	}
}

func TestCompareStrictPromotionRejectsTeacherAndSyntheticLabels(t *testing.T) {
	tests := []struct {
		name       string
		provenance LabelProvenance
		strength   LabelStrength
	}{
		{
			name:       "teacher",
			provenance: LabelProvenanceProductionTeacher,
			strength:   LabelStrengthTeacher,
		},
		{
			name:       "synthetic",
			provenance: LabelProvenanceSynthetic,
			strength:   LabelStrengthWeak,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			minimumGroups := minimumZeroDiscordanceGroups(ComparisonHarmMargin)
			records := comparisonContentRecords(minimumGroups, true)
			for i := range records {
				records[i].SliceIDs = append(
					records[i].SliceIDs,
					defaultHoldoutSliceID,
				)
				records[i].Label = LabelMetadata{
					Provenance:    test.provenance,
					Strength:      test.strength,
					PolicyVersion: "policy-v1",
				}
			}
			corpus := mustCorpus(t, records)
			control := comparisonContentResults(
				corpus,
				comparisonControlSystem(),
				func(int) bool { return true },
			)
			candidate := comparisonContentResults(
				corpus,
				comparisonCandidateSystem(),
				func(int) bool { return true },
			)
			options := comparisonTestOptions()
			options.PromotionMode = true

			report, err := Compare(corpus, control, candidate, options)
			if test.provenance == LabelProvenanceProductionTeacher {
				if err == nil ||
					!strings.Contains(err.Error(), "human_review/gold") {
					t.Fatalf("teacher comparison gate error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			if report.DiagnosticVerdict != ComparisonPass ||
				report.Promotion.Decision != ComparisonInconclusive ||
				report.Promotion.Eligible ||
				!containsComparisonReason(
					report.Promotion.ReasonCodes,
					"non_gold_labels",
				) {
				t.Fatalf("%s promotion report = %+v", test.name, report)
			}
		})
	}
}

func TestCompareRejectsResultSetMismatch(t *testing.T) {
	records := comparisonContentRecords(2, true)
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(corpus, comparisonControlSystem(), func(int) bool {
		return true
	})
	candidate := comparisonContentResults(corpus, comparisonCandidateSystem(), func(int) bool {
		return true
	})

	_, err := Compare(corpus, control, candidate[:1], comparisonTestOptions())
	if err == nil || !strings.Contains(err.Error(), "expected exactly 2 frozen cases, got 1") {
		t.Fatalf("Compare error = %v", err)
	}
}

func TestCompareRejectsDifferentEvaluatorBuild(t *testing.T) {
	records := comparisonContentRecords(2, true)
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(
		corpus,
		comparisonControlSystem(),
		func(int) bool { return true },
	)
	candidate := comparisonContentResults(
		corpus,
		comparisonCandidateSystem(),
		func(int) bool { return true },
	)
	candidate[0].EvaluatorBuildSHA256 =
		"2222222222222222222222222222222222222222222222222222222222222222"

	_, err := Compare(corpus, control, candidate, comparisonTestOptions())
	if err == nil || !strings.Contains(err.Error(), "different evaluator build") {
		t.Fatalf("Compare error = %v, want evaluator build mismatch", err)
	}
}

func TestCompareRejectsDifferentManifestIdentity(t *testing.T) {
	records := comparisonContentRecords(2, true)
	corpus := mustCorpus(t, records)
	control := comparisonContentResults(
		corpus,
		comparisonControlSystem(),
		func(int) bool { return true },
	)
	candidate := comparisonContentResults(
		corpus,
		comparisonCandidateSystem(),
		func(int) bool { return true },
	)
	for index := range candidate {
		candidate[index].ExecutionAudit.ManifestSHA256 =
			"4444444444444444444444444444444444444444444444444444444444444444"
	}

	_, err := Compare(corpus, control, candidate, comparisonTestOptions())
	if err == nil || !strings.Contains(err.Error(), "different manifest identities") {
		t.Fatalf("Compare error = %v, want manifest identity mismatch", err)
	}
}

func TestCompareCarriesAndRequiresOneCampaignIdentity(t *testing.T) {
	corpus := mustCorpus(t, comparisonContentRecords(2, true))
	control := comparisonContentResults(
		corpus,
		comparisonControlSystem(),
		func(int) bool { return true },
	)
	candidate := comparisonContentResults(
		corpus,
		comparisonCandidateSystem(),
		func(int) bool { return true },
	)
	plan := testCampaignPlan()
	planSHA := strings.Repeat("9", 64)
	controlBinding, err := plan.RunBinding(
		planSHA,
		"run-normalized-control-contentfilter",
	)
	if err != nil {
		t.Fatalf("control RunBinding: %v", err)
	}
	candidateBinding, err := plan.RunBinding(
		planSHA,
		"run-candidate-contentfilter",
	)
	if err != nil {
		t.Fatalf("candidate RunBinding: %v", err)
	}
	bind := func(binding *CampaignRunBinding, result ResultRecord) {
		binding.SystemID = result.System.SystemID
		binding.SystemManifestSHA256 = result.ExecutionAudit.ManifestSHA256
		binding.Corpus.SHA256 = corpus.SHA256
		binding.RouteSnapshot = &CampaignBoundArtifact{
			ArtifactID: "route-" + result.System.SystemID,
			SHA256:     result.ExecutionAudit.Route.SnapshotSHA256,
		}
		binding.ExactEndpointEvidence = true
	}
	bind(&controlBinding, control[0])
	bind(&candidateBinding, candidate[0])
	for index := range control {
		control[index].ExecutionAudit.Campaign =
			cloneCampaignRunBinding(&controlBinding)
		candidate[index].ExecutionAudit.Campaign =
			cloneCampaignRunBinding(&candidateBinding)
	}

	report, err := Compare(
		corpus,
		control,
		candidate,
		comparisonTestOptions(),
	)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if report.ControlCampaign == nil || report.CandidateCampaign == nil ||
		report.ControlCampaign.CampaignSHA256 != planSHA ||
		report.CandidateCampaign.CampaignSHA256 != planSHA {
		t.Fatalf("campaign report bindings = %+v / %+v", report.ControlCampaign, report.CandidateCampaign)
	}

	for index := range candidate {
		candidate[index].ExecutionAudit.Campaign.CampaignSHA256 =
			strings.Repeat("8", 64)
	}
	_, err = Compare(corpus, control, candidate, comparisonTestOptions())
	if err == nil || !strings.Contains(err.Error(), "different campaign identities") {
		t.Fatalf("Compare campaign mismatch error = %v", err)
	}
}

func TestCompareWithThresholdsUsesEachFrozenThreshold(t *testing.T) {
	corpus := mustCorpus(t, comparisonContentRecords(1, true))
	control := comparisonContentResults(
		corpus,
		comparisonControlSystem(),
		func(int) bool { return false },
	)
	candidate := comparisonContentResults(
		corpus,
		comparisonCandidateSystem(),
		func(int) bool { return false },
	)
	control[0].ContentFilter.Confidence = 0.80
	candidate[0].ContentFilter.Confidence = 0.80

	if _, err := Compare(
		corpus,
		control,
		candidate,
		comparisonTestOptions(),
	); err == nil || !strings.Contains(err.Error(), "drop action requires at least") {
		t.Fatalf("production-threshold Compare error = %v", err)
	}
	custom := ProductionThresholds()
	custom.ContentDropConfidence = 0.75
	report, err := CompareWithThresholds(
		corpus,
		control,
		candidate,
		comparisonTestOptions(),
		custom,
		custom,
	)
	if err != nil {
		t.Fatalf("CompareWithThresholds: %v", err)
	}
	if report.Options.ControlDecisionThresholds != custom ||
		report.Options.CandidateDecisionThresholds != custom {
		t.Fatalf(
			"comparison thresholds = control %+v candidate %+v, want %+v",
			report.Options.ControlDecisionThresholds,
			report.Options.CandidateDecisionThresholds,
			custom,
		)
	}
}

func TestGroupClusterBootstrapResamplesWholeGroupsDeterministically(t *testing.T) {
	observations := []pairedObservation{
		{groupID: "large", candidate: true},
		{groupID: "large", candidate: true},
		{groupID: "large", candidate: true},
		{groupID: "small", control: true},
	}
	const (
		replicates = 2_000
		seed       = 17
	)
	lowerA, upperA := clusterBootstrapBounds(observations, replicates, seed)
	lowerB, upperB := clusterBootstrapBounds(observations, replicates, seed)
	if lowerA != lowerB || upperA != upperB {
		t.Fatalf("bootstrap is not deterministic: (%v,%v) != (%v,%v)",
			lowerA,
			upperA,
			lowerB,
			upperB,
		)
	}
	// Drawing the small group twice produces -1 and drawing the large group
	// twice produces +1. Both have 25% probability under a two-cluster
	// bootstrap, so both must be beyond the 5th/95th percentiles. A row-level
	// bootstrap would not preserve this dependence structure.
	if lowerA != -1 || upperA != 1 {
		t.Fatalf("cluster bounds = (%v,%v), want (-1,1)", lowerA, upperA)
	}
}

func TestPrimaryIndicatorsCoverEveryTask(t *testing.T) {
	extraction := testExtraction("Expected")
	extractRecord := testExtractRecord("extract:case", MatcherExtractExpected{
		Acceptable: []MatcherExtraction{extraction},
	})
	extractCorpus := mustCorpus(t, []CorpusRecord{extractRecord})
	extractGood := extractResult(
		extractCorpus,
		extractRecord.CaseID,
		MatcherExtractActionExtract,
		extraction,
	)
	extractBad := extractGood
	badExtraction := testExtraction("Invented")
	extractBad.MatcherExtract = &MatcherExtractResult{
		Action:     MatcherExtractActionExtract,
		Extraction: &badExtraction,
	}
	assertIndicators(t, "matcher extract good", indicatorsForCase(extractRecord, extractGood), false, true)
	normalizedExtraction := extraction
	normalizedExtraction.Title = "Movie Part 2"
	normalizedGold := extraction
	normalizedGold.Title = "The Movie Part II"
	normalizedRecord := testExtractRecord("extract:normalized", MatcherExtractExpected{
		Acceptable: []MatcherExtraction{normalizedGold},
	})
	normalizedCorpus := mustCorpus(t, []CorpusRecord{normalizedRecord})
	normalizedResult := extractResult(
		normalizedCorpus,
		normalizedRecord.CaseID,
		MatcherExtractActionExtract,
		normalizedExtraction,
	)
	assertIndicators(
		t,
		"matcher extract production-normalized",
		indicatorsForCase(normalizedRecord, normalizedResult),
		false,
		true,
	)
	assertIndicators(t, "matcher extract bad", indicatorsForCase(extractRecord, extractBad), true, false)

	rerankRecord := testRerankRecord("rerank:case", MatcherRerankExpected{
		AcceptableTMDBIDs: []int64{101},
	})
	rerankCorpus := mustCorpus(t, []CorpusRecord{rerankRecord})
	rerankGood := rerankResult(
		rerankCorpus,
		rerankRecord.CaseID,
		MatcherRerankActionAttach,
		101,
	)
	rerankBad := rerankResult(
		rerankCorpus,
		rerankRecord.CaseID,
		MatcherRerankActionAttach,
		202,
	)
	assertIndicators(t, "matcher rerank good", indicatorsForCase(rerankRecord, rerankGood), false, true)
	assertIndicators(t, "matcher rerank bad", indicatorsForCase(rerankRecord, rerankBad), true, false)

	contentRecord := testContentRecord("content:case", LanguageEnglish)
	contentCorpus := mustCorpus(t, []CorpusRecord{contentRecord})
	contentGood := contentResult(
		contentCorpus,
		contentRecord.CaseID,
		ContentFilterActionKeep,
		boolPointer(true),
	)
	contentBad := contentResult(
		contentCorpus,
		contentRecord.CaseID,
		ContentFilterActionDrop,
		boolPointer(false),
	)
	assertIndicators(t, "content good", indicatorsForCase(contentRecord, contentGood), false, true)
	assertIndicators(t, "content bad", indicatorsForCase(contentRecord, contentBad), true, false)

	junkRecord := testJunkRecord("junk:case", JunkClassMovie)
	junkCorpus := mustCorpus(t, []CorpusRecord{junkRecord})
	junkGood := junkResult(
		junkCorpus,
		junkRecord.CaseID,
		JunkPurgeActionKeep,
		JunkVerdictRealMangled,
	)
	junkBad := junkResult(
		junkCorpus,
		junkRecord.CaseID,
		JunkPurgeActionJunk,
		JunkVerdictJunk,
	)
	assertIndicators(t, "junk good", indicatorsForCase(junkRecord, junkGood), false, true)
	assertIndicators(t, "junk bad", indicatorsForCase(junkRecord, junkBad), true, false)
}

func TestPrimaryIndicatorsTreatGoldUncertaintyAsAbstentionSuccess(t *testing.T) {
	content := testContentRecord("content:uncertain", LanguageEnglish)
	content.ContentFilter.Expected = ContentFilterExpected{AllowAbstain: true}
	contentCorpus := mustCorpus(t, []CorpusRecord{content})
	contentAbstain := contentResult(
		contentCorpus,
		content.CaseID,
		ContentFilterActionAbstain,
		nil,
	)
	contentKeep := contentResult(
		contentCorpus,
		content.CaseID,
		ContentFilterActionKeep,
		boolPointer(true),
	)
	assertExactIndicators(
		t,
		"content uncertain abstain",
		indicatorsForCase(content, contentAbstain),
		caseIndicators{successEligible: true, success: true},
	)
	assertExactIndicators(
		t,
		"content uncertain action",
		indicatorsForCase(content, contentKeep),
		caseIndicators{successEligible: true},
	)

	junk := testJunkRecord("junk:unsure", JunkClassUnresolved)
	junkCorpus := mustCorpus(t, []CorpusRecord{junk})
	junkAbstain := junkResult(
		junkCorpus,
		junk.CaseID,
		JunkPurgeActionAbstain,
		JunkVerdictUnsure,
	)
	junkKeep := junkResult(
		junkCorpus,
		junk.CaseID,
		JunkPurgeActionKeep,
		JunkVerdictRealMangled,
	)
	assertExactIndicators(
		t,
		"junk unsure abstain",
		indicatorsForCase(junk, junkAbstain),
		caseIndicators{successEligible: true, success: true},
	)
	assertExactIndicators(
		t,
		"junk unsure action",
		indicatorsForCase(junk, junkKeep),
		caseIndicators{successEligible: true},
	)
}

func TestMatcherRerankIndicatorsSeparateNonActionFromWrongAttachment(t *testing.T) {
	required := testRerankRecord("rerank:required", MatcherRerankExpected{
		AcceptableTMDBIDs: []int64{101},
	})
	requiredCorpus := mustCorpus(t, []CorpusRecord{required})

	requiredAbstain := rerankResult(
		requiredCorpus,
		required.CaseID,
		MatcherRerankActionAbstain,
		0,
	)
	assertExactIndicators(
		t,
		"required abstention",
		indicatorsForCase(required, requiredAbstain),
		caseIndicators{
			harmEligible:    true,
			successEligible: true,
		},
	)

	requiredError := errorResult(
		requiredCorpus,
		required.CaseID,
		TaskMatcherRerank,
		ResultStatusError,
		"timeout",
	)
	assertExactIndicators(
		t,
		"required runtime error",
		indicatorsForCase(required, requiredError),
		caseIndicators{
			harmEligible:    true,
			successEligible: true,
		},
	)

	optional := testRerankRecord(
		"rerank:optional",
		MatcherRerankExpected{AllowAbstain: true},
	)
	optionalCorpus := mustCorpus(t, []CorpusRecord{optional})
	optionalAbstain := rerankResult(
		optionalCorpus,
		optional.CaseID,
		MatcherRerankActionAbstain,
		0,
	)
	assertExactIndicators(
		t,
		"allowed abstention",
		indicatorsForCase(optional, optionalAbstain),
		caseIndicators{harmEligible: true},
	)
	if !goldOutcomeSuccess(optional, optionalAbstain) {
		t.Fatal("reviewed-gold allowed abstention was not safety success")
	}
	if goldOutcomeSuccess(required, requiredAbstain) {
		t.Fatal("wrong abstention on an attachment-required case was safety success")
	}

	wrongAttach := rerankResult(
		optionalCorpus,
		optional.CaseID,
		MatcherRerankActionAttach,
		101,
	)
	assertExactIndicators(
		t,
		"wrong attachment when abstention is required",
		indicatorsForCase(optional, wrongAttach),
		caseIndicators{harmEligible: true, harm: true},
	)
}

func comparisonContentRecords(count int, safety bool) []CorpusRecord {
	records := make([]CorpusRecord, 0, count)
	for index := 0; index < count; index++ {
		record := testContentRecord(
			fmt.Sprintf("content:%04d", index),
			LanguageEnglish,
		)
		record.GroupID = fmt.Sprintf("identity:%04d", index)
		if safety {
			record.SliceIDs = append(record.SliceIDs, defaultSafetySliceID)
		}
		records = append(records, record)
	}
	return records
}

func productionComparisonContentRecords(
	natural int,
	safety int,
	safetyAllowAbstain bool,
) []CorpusRecord {
	records := comparisonContentRecords(natural+safety, false)
	for index := range records {
		suite := "natural"
		if index >= natural {
			suite = "safety"
			if safetyAllowAbstain {
				records[index].ContentFilter.Expected = ContentFilterExpected{
					AllowAbstain: true,
				}
			}
		}
		records[index].SliceIDs = []string{
			"all",
			"fixture",
			"suite:" + suite,
		}
	}
	return records
}

func comparisonContentResults(
	corpus Corpus,
	system SystemDescriptor,
	keep func(index int) bool,
) []ResultRecord {
	results := make([]ResultRecord, 0, len(corpus.Records))
	for index, record := range corpus.Records {
		action := ContentFilterActionDrop
		isEnglish := false
		if keep(index) {
			action = ContentFilterActionKeep
			isEnglish = true
		}
		results = append(results, ResultRecord{
			SchemaVersion:         SchemaVersion,
			CorpusSHA256:          corpus.SHA256,
			RequestContractSHA256: testRequestContractSHA,
			EvaluatorBuildSHA256:  testEvaluatorBuildSHA,
			ExecutionAudit:        testOpenRouterExecutionAudit(),
			CaseID:                record.CaseID,
			Task:                  record.Task,
			System:                system,
			Status:                ResultStatusOK,
			ContentFilter: &ContentFilterResult{
				Action:     action,
				IsEnglish:  &isEnglish,
				Confidence: 0.9,
			},
		})
	}
	return results
}

func comparisonControlSystem() SystemDescriptor {
	system := testSystem()
	system.SystemID = "control-system"
	system.Model = "control/model"
	return system
}

func comparisonCandidateSystem() SystemDescriptor {
	system := testSystem()
	system.SystemID = "candidate-system"
	system.Model = "candidate/model"
	return system
}

func comparisonTestOptions() ComparisonOptions {
	return ComparisonOptions{
		BootstrapReplicates:  1_000,
		BootstrapSeed:        12345,
		SafetySliceID:        defaultSafetySliceID,
		ClosureBoundHoldout:  true,
		EvaluatorBuildSHA256: testEvaluatorBuildSHA,
	}
}

func onlyTaskComparison(t *testing.T, report ComparisonReport) TaskComparison {
	t.Helper()
	if len(report.Tasks) != 1 {
		t.Fatalf("task count = %d, want 1", len(report.Tasks))
	}
	return report.Tasks[0]
}

func assertIndicators(
	t *testing.T,
	name string,
	indicators caseIndicators,
	wantHarm bool,
	wantSuccess bool,
) {
	t.Helper()
	if !indicators.harmEligible || !indicators.successEligible ||
		indicators.harm != wantHarm || indicators.success != wantSuccess {
		t.Fatalf("%s indicators = %+v, want harm=%v success=%v",
			name,
			indicators,
			wantHarm,
			wantSuccess,
		)
	}
}

func assertExactIndicators(
	t *testing.T,
	name string,
	got caseIndicators,
	want caseIndicators,
) {
	t.Helper()
	if got != want {
		t.Fatalf("%s indicators = %+v, want %+v", name, got, want)
	}
}

func containsComparisonReason(reasons []string, wanted string) bool {
	for _, reason := range reasons {
		if reason == wanted {
			return true
		}
	}
	return false
}
