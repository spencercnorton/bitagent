package llmeval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildPromotionBundleSelectsCheapestCompleteCandidate(t *testing.T) {
	input := promotionBundleFixture(t)

	bundle, err := BuildPromotionBundle(input)
	require.NoError(t, err)
	require.Equal(
		t,
		input.GoldClosureManifest.PlanID,
		input.CampaignPlan.Plan.CorpusPlanID,
	)
	require.Equal(
		t,
		input.GoldClosureManifest.PlanSHA256,
		input.CampaignPlan.Plan.CorpusPlanSHA256,
	)
	require.Equal(
		t,
		input.CorpusPlanSHA256,
		bundle.Bindings.CorpusPlanSHA256,
	)
	require.True(t, bundle.ProtocolComplete, "bundle reasons: %v", bundle.ReasonCodes)
	require.True(t, bundle.Eligible)
	require.Equal(t, ComparisonPass, bundle.Decision)
	require.Equal(t, "candidate", bundle.SelectedSystemID)
	require.Equal(
		t,
		input.PromotionRosterSHA256,
		bundle.Bindings.PromotionRosterSHA256,
	)
	require.Len(t, bundle.HolmFamilies, 1)

	control := requirePromotionSystemReport(t, bundle, "control")
	productionControl := requirePromotionSystemReport(
		t,
		bundle,
		"production-control",
	)
	candidate := requirePromotionSystemReport(t, bundle, "candidate")
	require.Equal(t, PromotionRoleControl, control.Role)
	require.Equal(t, PromotionRoleControl, productionControl.Role)
	require.Equal(t, PromotionRoleCandidate, candidate.Role)
	require.True(t, candidate.CoreGatesPassed)
	require.True(t, candidate.HolmPassed)
	require.True(t, candidate.FirstPassSchema.Passed)
	require.Equal(t, 1.0, candidate.FirstPassSchema.FirstPassValidity)
	require.True(t, candidate.Calibration.Passed)
	require.True(t, candidate.Instability.DeterministicControlsProven)
	require.False(t, candidate.Instability.RepeatsRequired)
	require.True(t, candidate.RequestShape.OneCaseVerified)
	require.True(t, candidate.Latency.Complete)
	require.True(t, candidate.Latency.Passed)
	require.Len(t, candidate.Latency.Comparisons, 2)
	require.True(t, candidate.HarmUtility.EvidenceComplete)
	require.True(t, candidate.HarmUtility.Passed)
	require.Len(t, candidate.HarmUtility.ControlComparisons, 2)
	require.True(t, candidate.Calibration.EvidenceComplete)
	require.True(t, candidate.SelectiveRisk.EvidenceComplete)
	require.True(t, candidate.SelectiveRisk.Passed)
	require.Len(t, candidate.SelectiveRisk.ControlComparisons, 2)
	require.Len(t, candidate.CriticalStrata, 2)
	for _, stratum := range candidate.CriticalStrata {
		require.True(t, stratum.Passed)
	}
	require.False(t, candidate.CriticalStrata[1].HarmApplicable)
	require.Equal(
		t,
		"no_gold_cases_in_task_harm_denominator",
		candidate.CriticalStrata[1].HarmNotApplicableReason,
	)
	require.True(t, candidate.CostGate.EvidenceComplete)
	require.True(t, candidate.CostGate.CostPer1000CasesPassed)
	require.True(t, candidate.CostGate.ProjectedMonthlyPassed)
	require.True(t, candidate.CostGate.CostPerCorrectSafeActionPassed)
	require.True(t, candidate.CostGate.Passed)
	require.Len(t, candidate.CostGate.ControlComparisons, 2)
	require.True(t, candidate.CampaignGatesPassed)
	require.Equal(
		t,
		input.CampaignPlan.SHA256,
		candidate.CampaignRun.CampaignSHA256,
	)
	require.True(t, candidate.Eligible)
	require.NotNil(t, candidate.Comparison)
	require.False(t, candidate.Comparison.Promotion.ProtocolComplete)
	require.NotNil(t, candidate.ProductionComparison)
	require.False(
		t,
		candidate.ProductionComparison.Promotion.ProtocolComplete,
	)
	require.NotEmpty(t, candidate.ScoreReportSHA256)
	require.NotEmpty(t, candidate.ComparisonReportSHA256)
	require.NotEmpty(t, candidate.ProductionComparisonReportSHA256)
	require.Nil(t, candidate.Cost.OpenAIBatchNormalizedUSD)
	require.NotNil(t, control.Cost.OpenAIBatchNormalizedUSD)
	require.NotNil(t, control.Cost.OpenAIFlexNormalizedUSD)
	require.InDelta(t, 1.055, candidate.Cost.RecurringCashMultiplier, 1e-12)
	require.InDelta(
		t,
		candidate.Cost.MeasuredProviderCreditsUSD*1.055,
		candidate.Cost.MeasuredCashCostUSD,
		1e-12,
	)
	require.InDelta(
		t,
		0.80,
		candidate.Cost.OpenRouterOneTimeMinimumFeeUSD,
		1e-12,
	)
	require.Less(
		t,
		candidate.Cost.ProjectedThirtyDayCashCostUSD,
		control.Cost.ProjectedThirtyDayCashCostUSD,
	)
	for _, family := range bundle.HolmFamilies {
		require.Len(t, family.Hypotheses, 1)
		require.Len(t, family.Hypotheses[0].Components, 6)
		require.True(t, family.Hypotheses[0].Passed)
		maxComponentP := 0.0
		for _, component := range family.Hypotheses[0].Components {
			if component.UnadjustedP > maxComponentP {
				maxComponentP = component.UnadjustedP
			}
		}
		require.InDelta(
			t,
			maxComponentP,
			family.Hypotheses[0].UnadjustedP,
			1e-15,
		)
	}
}

func TestBuildPromotionBundlePromotesMatcherExtractWithoutConfidence(t *testing.T) {
	input := promotionMatcherExtractBundleFixture(t)
	require.Nil(t, input.CampaignPlan.Plan.EffectivenessGates[0].Calibration)
	require.Nil(t, input.CampaignPlan.Plan.EffectivenessGates[0].SelectiveRisk)

	bundle, err := BuildPromotionBundle(input)
	require.NoError(t, err)
	require.True(t, bundle.ProtocolComplete)
	require.True(t, bundle.Eligible)
	require.Equal(t, ComparisonPass, bundle.Decision)
	require.Equal(t, "candidate", bundle.SelectedSystemID)

	candidate := requirePromotionSystemReport(t, bundle, "candidate")
	require.False(t, candidate.Calibration.Applicable)
	require.True(t, candidate.Calibration.EvidenceComplete)
	require.True(t, candidate.Calibration.Passed)
	require.NotEmpty(t, candidate.Calibration.NotApplicableReason)
	require.Equal(t, CampaignCalibrationGate{}, candidate.Calibration.Gate)
	require.Empty(t, candidate.Calibration.Suites)
	require.False(t, candidate.SelectiveRisk.Applicable)
	require.True(t, candidate.SelectiveRisk.EvidenceComplete)
	require.True(t, candidate.SelectiveRisk.Passed)
	require.NotEmpty(t, candidate.SelectiveRisk.NotApplicableReason)
	require.Equal(t, CampaignSelectiveRiskGate{}, candidate.SelectiveRisk.Gate)
	require.Empty(t, candidate.SelectiveRisk.ControlComparisons)
	require.True(t, candidate.CampaignGatesPassed)
	require.True(t, candidate.Eligible)
	require.NotContains(
		t,
		candidate.ReasonCodes,
		"campaign_calibration_evidence_unavailable",
	)
	require.NotContains(
		t,
		candidate.ReasonCodes,
		"campaign_selective_risk_evidence_unavailable",
	)
}

func TestPromotionV2RequiresExactUntamperedCampaignPlan(t *testing.T) {
	t.Run("must use exact-byte reader", func(t *testing.T) {
		input := promotionBundleFixture(t)
		input.CampaignPlan.canonicalSHA256 = ""

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "must be loaded from exact bytes")
	})

	t.Run("plan mutated after read", func(t *testing.T) {
		input := promotionBundleFixture(t)
		input.CampaignPlan.Plan.EffectivenessGates[0].Latency.MaxP95MS--

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "mutated after its exact bytes were read")
	})

	t.Run("artifact sha tamper", func(t *testing.T) {
		input := promotionBundleFixture(t)
		input.CampaignPlan.SHA256 = strings.Repeat("e", 64)

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "SHA-256 was mutated")
	})
}

func TestPromotionV2RejectsPostHoldoutBootstrapShopping(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*PromotionBundleInput)
		wantErr string
	}{
		{
			name: "replicates",
			mutate: func(input *PromotionBundleInput) {
				input.ComparisonOptions.BootstrapReplicates =
					input.CampaignPlan.Plan.EffectivenessGates[0].
						Inference.BootstrapReplicates + 1
			},
			wantErr: "bootstrap_replicates differs from the preregistered campaign",
		},
		{
			name: "seed",
			mutate: func(input *PromotionBundleInput) {
				input.ComparisonOptions.BootstrapSeed =
					input.CampaignPlan.Plan.EffectivenessGates[0].
						Inference.BootstrapSeed + 1
			},
			wantErr: "bootstrap_seed differs from the preregistered campaign",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := promotionBundleFixture(t)
			test.mutate(&input)
			bundle, err := BuildPromotionBundle(input)
			require.ErrorContains(t, err, test.wantErr)
			require.False(t, bundle.ProtocolComplete)
		})
	}
}

func TestPromotionV2BindsCampaignCorpusPlanToGoldClosure(t *testing.T) {
	tests := []struct {
		name string
		edit func(*CampaignPlan)
		want string
	}{
		{
			name: "plan id mismatch",
			edit: func(plan *CampaignPlan) {
				plan.CorpusPlanID = "different-corpus-plan"
			},
			want: "corpus plan identity does not match campaign binding",
		},
		{
			name: "plan sha mismatch",
			edit: func(plan *CampaignPlan) {
				plan.CorpusPlanSHA256 = strings.Repeat("e", 64)
			},
			want: "corpus plan identity does not match campaign binding",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := promotionBundleFixture(t)
			plan := input.CampaignPlan.Plan
			test.edit(&plan)
			raw, err := json.Marshal(plan)
			require.NoError(t, err)
			campaign, err := ReadPromotionCampaignPlanEvidence(
				bytes.NewReader(raw),
			)
			require.NoError(t, err)
			input.CampaignPlan = campaign

			bundle, err := BuildPromotionBundle(input)
			require.ErrorContains(t, err, test.want)
			require.False(t, bundle.ProtocolComplete)
		})
	}
}

func TestPromotionV2ConsumesExactCorpusPlanSafetyRequirements(t *testing.T) {
	tests := []struct {
		name string
		edit func(*CampaignEffectivenessGate)
		want string
	}{
		{
			name: "required slice omitted",
			edit: func(gate *CampaignEffectivenessGate) {
				gate.CriticalStrata = gate.CriticalStrata[1:]
			},
			want: "missing corpus-plan-required safety count slice \"hard_must_keep_english\"",
		},
		{
			name: "required count underdeclared",
			edit: func(gate *CampaignEffectivenessGate) {
				gate.CriticalStrata[1].MinimumCases = 499
			},
			want: "minimum_cases 499 is below corpus-plan-required safety count 500",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := promotionBundleFixture(t)
			plan := input.CampaignPlan.Plan
			test.edit(&plan.EffectivenessGates[0])
			raw, err := json.Marshal(plan)
			require.NoError(t, err)
			campaign, err := ReadPromotionCampaignPlanEvidence(
				bytes.NewReader(raw),
			)
			require.NoError(t, err)
			input.CampaignPlan = campaign

			bundle, err := BuildPromotionBundle(input)
			require.ErrorContains(t, err, test.want)
			require.False(t, bundle.ProtocolComplete)
		})
	}
}

func TestPromotionV2RejectsCorpusPlanBytesChangedAfterFreeze(t *testing.T) {
	input := promotionBundleFixture(t)
	plan, _, err := ReadCorpusPlan(bytes.NewReader(input.CorpusPlanBytes))
	require.NoError(t, err)
	for index := range plan.Tasks {
		if plan.Tasks[index].Task == TaskContentFilter {
			plan.Tasks[index].RequiredSafetyCounts["hard_must_keep_english"] = 1
		}
	}
	mutated, err := json.Marshal(plan)
	require.NoError(t, err)
	input.CorpusPlanBytes = mutated

	bundle, err := BuildPromotionBundle(input)
	require.ErrorContains(
		t,
		err,
		"corpus_plan_sha256 does not match the exact supplied corpus plan bytes",
	)
	require.False(t, bundle.ProtocolComplete)
}

func TestPromotionCriticalStratumHarmApplicabilityIsGoldDerived(
	t *testing.T,
) {
	input := promotionBundleFixture(t)
	corpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
	require.NoError(t, err)
	gates := input.CampaignPlan.Plan.EffectivenessGates[0].CriticalStrata

	// Withhold every result. Wanted-English rows remain mechanically inside
	// the task harm denominator, so missing/corrupt caller evidence cannot turn
	// their harm gate into N/A. The non-English complement is structurally
	// outside that denominator, but still fails its independent utility gate.
	reports := promotionCampaignCriticalStrata(corpus, nil, gates)
	require.Len(t, reports, 2)
	require.True(t, reports[0].HarmApplicable)
	require.Equal(t, 3_000, reports[0].Harm.Denominator)
	require.True(t, reports[0].Harm.EvidenceComplete)
	require.Empty(t, reports[0].HarmNotApplicableReason)

	require.False(t, reports[1].HarmApplicable)
	require.Equal(t, 0, reports[1].Harm.Denominator)
	require.Equal(
		t,
		"no_gold_cases_in_task_harm_denominator",
		reports[1].HarmNotApplicableReason,
	)
	require.True(t, reports[1].Harm.Passed)
	require.False(t, reports[1].Utility.Passed)
	require.False(t, reports[1].Passed)
}

func TestPromotionV2RepeatCampaignKeepsGoldClosureCorpusPlanBinding(
	t *testing.T,
) {
	input := promotionBundleFixture(t)
	addPromotionRepeatCampaignFixture(t, &input)
	require.NotNil(t, input.RepeatCampaignPlan)
	repeatPlan := input.RepeatCampaignPlan.Plan
	repeatPlan.CorpusPlanSHA256 = strings.Repeat("e", 64)
	raw, err := json.Marshal(repeatPlan)
	require.NoError(t, err)
	repeatCampaign, err := ReadPromotionCampaignPlanEvidence(
		bytes.NewReader(raw),
	)
	require.NoError(t, err)
	input.RepeatCampaignPlan = &repeatCampaign

	bundle, err := BuildPromotionBundle(input)
	require.ErrorContains(
		t,
		err,
		"repeat plan bindings: corpus plan identity does not match campaign binding",
	)
	require.False(t, bundle.ProtocolComplete)
}

func TestPromotionV2RequiresEveryPrimaryCampaignBinding(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		input := promotionBundleFixture(t)
		for index := range input.Systems[1].Results {
			input.Systems[1].Results[index].ExecutionAudit.Campaign = nil
		}
		refreshPromotionResultsBinding(t, &input.Systems[1])

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "missing campaign run binding")
	})

	t.Run("tampered", func(t *testing.T) {
		input := promotionBundleFixture(t)
		binding := *input.Systems[1].Results[0].ExecutionAudit.Campaign
		binding.RunID = "promotion-holdout-control"
		for index := range input.Systems[1].Results {
			bound := binding
			input.Systems[1].Results[index].ExecutionAudit.Campaign = &bound
		}
		refreshPromotionResultsBinding(t, &input.Systems[1])

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "campaign run binding differs")
	})
}

func TestPromotionV2RequiresCompletePrimaryRequestTiming(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		input := promotionBundleFixture(t)
		input.Systems[1].Results[0].RequestTiming = nil
		refreshPromotionResultsBinding(t, &input.Systems[1])

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "missing primary request timing")
	})

	t.Run("deadline mismatch", func(t *testing.T) {
		input := promotionBundleFixture(t)
		input.Systems[1].Results[0].RequestTiming.DeadlineMS--
		refreshPromotionResultsBinding(t, &input.Systems[1])

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "differs from campaign and manifest deadline")
	})
}

func TestPromotionV2RequiresEveryRepeatCampaignBinding(t *testing.T) {
	t.Run("missing repeat plan", func(t *testing.T) {
		input := promotionBundleFixture(t)
		addPromotionRepeatCampaignFixture(t, &input)
		input.RepeatCampaignPlan = nil

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "repeat campaign plan is required")
	})

	t.Run("missing row binding", func(t *testing.T) {
		input := promotionBundleFixture(t)
		candidateIndex := addPromotionRepeatCampaignFixture(t, &input)
		repeat := input.Systems[candidateIndex].RepeatResults[0]
		for index := range repeat {
			repeat[index].ExecutionAudit.Campaign = nil
		}
		input.Systems[candidateIndex].RepeatResults[0] = repeat
		input.Systems[candidateIndex].RepeatAttemptEvidence[0] =
			promotionAttemptFixture(
				t,
				mustPromotionRepeatSample(t, input),
				promotionFixtureConfig(
					t,
					input,
					input.Systems[candidateIndex].SystemID,
				),
				repeat,
			)

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "missing campaign run binding")
	})

	t.Run("tampered row binding", func(t *testing.T) {
		input := promotionBundleFixture(t)
		candidateIndex := addPromotionRepeatCampaignFixture(t, &input)
		repeat := input.Systems[candidateIndex].RepeatResults[0]
		binding := *repeat[0].ExecutionAudit.Campaign
		binding.RunID = "promotion-repeat-candidate-2"
		for index := range repeat {
			bound := binding
			repeat[index].ExecutionAudit.Campaign = &bound
		}
		input.Systems[candidateIndex].RepeatResults[0] = repeat
		input.Systems[candidateIndex].RepeatAttemptEvidence[0] =
			promotionAttemptFixture(
				t,
				mustPromotionRepeatSample(t, input),
				promotionFixtureConfig(
					t,
					input,
					input.Systems[candidateIndex].SystemID,
				),
				repeat,
			)

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "campaign run binding differs")
	})
}

func TestPromotionV2EnforcesCampaignLatencyGates(t *testing.T) {
	t.Run("deadline overrun counts as timeout", func(t *testing.T) {
		input := promotionBundleFixture(t)
		for index := 0; index < 25; index++ {
			input.Systems[1].Results[index].RequestTiming.ElapsedMS = 8_001
		}
		refreshPromotionResultsBinding(t, &input.Systems[1])

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		candidate := requirePromotionSystemReport(t, bundle, "candidate")
		require.Equal(t, 25, candidate.Latency.DeadlineExceededCount)
		require.Equal(t, 25, candidate.Latency.TimeoutCount)
		require.Greater(
			t,
			candidate.Latency.TimeoutUpperConfidenceBound,
			candidate.Latency.Gate.MaxTimeoutUCB,
		)
		require.Equal(t, int64(500), candidate.Latency.P99MS)
		require.False(t, candidate.Latency.AbsoluteGatePassed)
		require.False(t, candidate.CampaignGatesPassed)
		require.False(t, candidate.Eligible)
		require.Contains(
			t,
			candidate.ReasonCodes,
			"campaign_latency_gate_not_passed",
		)
	})

	t.Run("absolute percentile", func(t *testing.T) {
		input := promotionBundleFixture(t)
		for index := range input.Systems[1].Results {
			input.Systems[1].Results[index].RequestTiming.ElapsedMS = 5_000
		}
		refreshPromotionResultsBinding(t, &input.Systems[1])

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		require.True(t, bundle.ProtocolComplete)
		require.False(t, bundle.Eligible)
		candidate := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, candidate.Latency.Complete)
		require.False(t, candidate.Latency.AbsoluteGatePassed)
		require.False(t, candidate.Latency.Passed)
		require.False(t, candidate.Eligible)
		require.Contains(t, candidate.ReasonCodes, "campaign_latency_gate_not_passed")
	})

	t.Run("relative p95 against both controls", func(t *testing.T) {
		input := promotionBundleFixture(t)
		for index := range input.Systems[1].Results {
			input.Systems[1].Results[index].RequestTiming.ElapsedMS = 900
		}
		refreshPromotionResultsBinding(t, &input.Systems[1])

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		candidate := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, candidate.Latency.AbsoluteGatePassed)
		require.False(t, candidate.Latency.Passed)
		require.Len(t, candidate.Latency.Comparisons, 2)
		for _, comparison := range candidate.Latency.Comparisons {
			require.False(t, comparison.RelativeGatePassed)
			require.False(t, comparison.Passed)
		}
		require.False(t, candidate.Eligible)
	})
}

func TestPromotionV2EnforcesCampaignEffectivenessGateFamilies(t *testing.T) {
	t.Run("absolute and relative harm", func(t *testing.T) {
		input := promotionBundleFixture(t)
		corpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
		require.NoError(t, err)
		changed := 0
		for index, record := range corpus.Records {
			if record.ContentFilter.Expected.Language != LanguageEnglish {
				continue
			}
			input.Systems[1].Results[index].ContentFilter.Action =
				ContentFilterActionDrop
			predictedEnglish := false
			input.Systems[1].Results[index].ContentFilter.IsEnglish =
				&predictedEnglish
			changed++
			if changed == 20 {
				break
			}
		}
		require.Equal(t, 20, changed)
		refreshPromotionResultsBinding(t, &input.Systems[1])

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		candidate := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, candidate.HarmUtility.EvidenceComplete)
		require.Equal(t, 20, candidate.HarmUtility.SeverityOneErrors)
		require.False(t, candidate.HarmUtility.Harm.Passed)
		require.False(t, candidate.HarmUtility.AbsolutePassed)
		require.Len(t, candidate.HarmUtility.ControlComparisons, 2)
		for _, comparison := range candidate.HarmUtility.ControlComparisons {
			require.True(t, comparison.EvidenceComplete)
			require.False(t, comparison.HarmPassed)
		}
		require.False(t, candidate.CampaignGatesPassed)
		require.Contains(
			t,
			candidate.ReasonCodes,
			"campaign_harm_utility_gate_not_passed",
		)
	})

	t.Run("absolute task utility", func(t *testing.T) {
		input := promotionBundleFixture(t)
		corpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
		require.NoError(t, err)
		changed := 0
		for index, record := range corpus.Records {
			if record.ContentFilter.Expected.Language != LanguageNonEnglish {
				continue
			}
			input.Systems[1].Results[index].ContentFilter.Action =
				ContentFilterActionKeep
			predictedEnglish := true
			input.Systems[1].Results[index].ContentFilter.IsEnglish =
				&predictedEnglish
			changed++
			if changed == 400 {
				break
			}
		}
		require.Equal(t, 400, changed)
		refreshPromotionResultsBinding(t, &input.Systems[1])

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		candidate := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, candidate.HarmUtility.Harm.Passed)
		require.InDelta(t, 0.8, candidate.HarmUtility.Utility.Rate, 1e-12)
		require.False(t, candidate.HarmUtility.Utility.Passed)
		require.False(t, candidate.HarmUtility.AbsolutePassed)
		require.False(t, candidate.CampaignGatesPassed)
	})

	t.Run("schema repair maximum", func(t *testing.T) {
		input := promotionBundleFixture(t)
		candidate := &input.Systems[1]
		config := promotionFixtureConfig(t, input, candidate.SystemID)
		initial := promotionUsageForTest(
			config, "campaign:schema:initial", 40, 4,
		)
		repair := promotionUsageForTest(
			config, "campaign:schema:repair", 60, 6,
		)
		aggregate, err := sumPromotionUsage(initial, repair)
		require.NoError(t, err)
		candidate.Results[0].Usage = aggregate
		candidate.AttemptEvidence.Attempts[0] = PromotionAttempt{
			CaseID: candidate.Results[0].CaseID, Sequence: 1,
			Kind: PromotionAttemptInitial, Status: ResultStatusSchemaError,
			Usage: initial,
		}
		candidate.AttemptEvidence.Attempts = append(
			candidate.AttemptEvidence.Attempts,
			PromotionAttempt{
				CaseID: candidate.Results[0].CaseID, Sequence: 2,
				Kind: PromotionAttemptSchemaRepair, Status: ResultStatusOK,
				Usage: repair,
			},
		)
		rewritePromotionCampaignGate(t, &input, func(gate *CampaignEffectivenessGate) {
			gate.SchemaReliability.MaxSchemaRepairsPerCase = 0
		})

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		report := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, report.FirstPassSchema.EvidenceComplete)
		require.Equal(t, 1, report.FirstPassSchema.MaximumRepairsObserved)
		require.GreaterOrEqual(
			t,
			report.FirstPassSchema.FirstPassValidity,
			report.FirstPassSchema.Gate.MinFirstPassSchemaValidRate,
		)
		require.False(t, report.FirstPassSchema.Passed)
		require.False(t, report.CampaignGatesPassed)
		require.Contains(
			t,
			report.ReasonCodes,
			"campaign_schema_reliability_gate_not_passed",
		)
	})

	t.Run("calibration threshold is a gate failure, not missing evidence", func(t *testing.T) {
		input := promotionBundleFixture(t)
		rewritePromotionCampaignGate(t, &input, func(gate *CampaignEffectivenessGate) {
			gate.Calibration.MaxError = 0.005
		})

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		require.True(t, bundle.ProtocolComplete)
		report := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, report.Calibration.EvidenceComplete)
		require.False(t, report.Calibration.Passed)
		require.True(t, report.EvidenceComplete)
		require.False(t, report.CampaignGatesPassed)
		require.Contains(
			t,
			report.ReasonCodes,
			"campaign_calibration_gate_not_passed",
		)
		require.NotContains(
			t,
			report.ReasonCodes,
			"campaign_calibration_evidence_unavailable",
		)
	})

	t.Run("selective risk at incumbent coverage", func(t *testing.T) {
		input := promotionBundleFixture(t)
		corpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
		require.NoError(t, err)
		changed := 0
		for index, record := range corpus.Records {
			if record.ContentFilter.Expected.Language != LanguageNonEnglish {
				continue
			}
			input.Systems[1].Results[index].ContentFilter.Action =
				ContentFilterActionKeep
			predictedEnglish := true
			input.Systems[1].Results[index].ContentFilter.IsEnglish =
				&predictedEnglish
			changed++
			if changed == 50 {
				break
			}
		}
		require.Equal(t, 50, changed)
		refreshPromotionResultsBinding(t, &input.Systems[1])

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		report := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, report.SelectiveRisk.EvidenceComplete)
		require.InDelta(t, 0.0125, report.SelectiveRisk.AURC, 1e-12)
		require.Len(t, report.SelectiveRisk.ControlComparisons, 2)
		for _, comparison := range report.SelectiveRisk.ControlComparisons {
			require.True(t, comparison.EvidenceComplete)
			require.True(t, comparison.CoveragePassed)
			require.False(t, comparison.RiskPassed)
		}
		require.False(t, report.SelectiveRisk.Passed)
		require.Contains(
			t,
			report.ReasonCodes,
			"campaign_selective_risk_gate_not_passed",
		)
	})

	t.Run("missing critical stratum fails closed", func(t *testing.T) {
		input := promotionBundleFixture(t)
		rewritePromotionCampaignGate(t, &input, func(gate *CampaignEffectivenessGate) {
			missing := gate.CriticalStrata[0]
			missing.SliceID = "missing_critical_stratum"
			gate.CriticalStrata = append(gate.CriticalStrata, missing)
		})

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		require.False(t, bundle.ProtocolComplete)
		report := requirePromotionSystemReport(t, bundle, "candidate")
		require.Len(t, report.CriticalStrata, 3)
		require.Zero(t, report.CriticalStrata[2].Cases)
		require.False(t, report.CriticalStrata[2].EvidenceComplete)
		require.False(t, report.EvidenceComplete)
		require.False(t, report.CampaignGatesPassed)
		require.Contains(
			t,
			report.ReasonCodes,
			"campaign_critical_strata_evidence_incomplete",
		)
	})

	t.Run("cost ceiling", func(t *testing.T) {
		input := promotionBundleFixture(t)
		rewritePromotionCampaignGate(t, &input, func(gate *CampaignEffectivenessGate) {
			gate.Cost.MaxCostPer1000CasesMicroUSD = 500
		})

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		report := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, report.CostGate.EvidenceComplete)
		require.Greater(t, report.CostGate.CostPer1000CasesMicroUSD, 500.0)
		require.False(t, report.CostGate.CostPer1000CasesPassed)
		require.True(t, report.CostGate.ProjectedMonthlyPassed)
		require.True(t, report.CostGate.CostPerCorrectSafeActionPassed)
		require.False(t, report.CostGate.AbsolutePassed)
		require.False(t, report.CampaignGatesPassed)
		require.Contains(
			t,
			report.ReasonCodes,
			"campaign_cost_gate_not_passed",
		)
	})

	t.Run("balanced utility relative bound", func(t *testing.T) {
		input := promotionBundleFixture(t)
		corpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
		require.NoError(t, err)
		for index, record := range corpus.Records {
			if record.ContentFilter.Expected.Language == LanguageNonEnglish {
				input.Systems[1].Results[index].ContentFilter.Action =
					ContentFilterActionKeep
				predictedEnglish := true
				input.Systems[1].Results[index].ContentFilter.IsEnglish =
					&predictedEnglish
				break
			}
		}
		rewritePromotionCampaignGate(t, &input, func(gate *CampaignEffectivenessGate) {
			gate.HarmUtility.MinUtilityLCBDeltaVsControl = 0
		})

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		report := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, report.HarmUtility.AbsolutePassed)
		require.Len(t, report.HarmUtility.ControlComparisons, 2)
		for _, comparison := range report.HarmUtility.ControlComparisons {
			require.True(t, comparison.EvidenceComplete)
			require.Less(t, comparison.UtilityLCBDelta, 0.0)
			require.False(t, comparison.UtilityPassed)
		}
		require.False(t, report.HarmUtility.Passed)
		require.False(t, report.CampaignGatesPassed)
	})

	t.Run("harmful repeat flip", func(t *testing.T) {
		input := promotionBundleFixture(t)
		candidateIndex := addPromotionRepeatCampaignFixture(t, &input)
		sample := mustPromotionRepeatSample(t, input)
		repeat := input.Systems[candidateIndex].RepeatResults[0]
		changed := false
		for index, record := range sample.Records {
			if record.ContentFilter.Expected.Language != LanguageEnglish {
				continue
			}
			repeat[index].ContentFilter.Action = ContentFilterActionDrop
			predictedEnglish := false
			repeat[index].ContentFilter.IsEnglish = &predictedEnglish
			changed = true
			break
		}
		require.True(t, changed)
		input.Systems[candidateIndex].RepeatResults[0] = repeat
		input.Systems[candidateIndex].RepeatAttemptEvidence[0] =
			promotionAttemptFixture(
				t,
				sample,
				promotionFixtureConfig(
					t,
					input,
					input.Systems[candidateIndex].SystemID,
				),
				repeat,
			)

		bundle, err := BuildPromotionBundle(input)
		require.NoError(t, err)
		report := requirePromotionSystemReport(t, bundle, "candidate")
		require.True(t, report.Instability.EvidenceComplete)
		require.Equal(t, 1, report.Instability.HarmfulRepeatFlips)
		require.False(t, report.Instability.Passed)
		require.False(t, report.CampaignGatesPassed)
		require.Contains(
			t,
			report.ReasonCodes,
			"campaign_stability_gate_not_passed",
		)
	})
}

func refreshPromotionResultsBinding(
	t *testing.T,
	evidence *PromotionSystemEvidence,
) {
	t.Helper()
	resultsSHA, err := ResultsIdentity(evidence.Results)
	require.NoError(t, err)
	evidence.AttemptEvidence.ResultsSHA256 = resultsSHA
}

func rewritePromotionCampaignGate(
	t *testing.T,
	input *PromotionBundleInput,
	edit func(*CampaignEffectivenessGate),
) {
	t.Helper()
	require.Nil(t, input.RepeatCampaignPlan)
	plan := input.CampaignPlan.Plan
	require.Len(t, plan.EffectivenessGates, 1)
	edit(&plan.EffectivenessGates[0])
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	evidence, err := ReadPromotionCampaignPlanEvidence(bytes.NewReader(raw))
	require.NoError(t, err)
	input.CampaignPlan = evidence
	for index := range input.Systems {
		system := &input.Systems[index]
		binding, bindingErr := evidence.Plan.RunBinding(
			evidence.SHA256,
			"promotion-holdout-"+system.SystemID,
		)
		require.NoError(t, bindingErr)
		for resultIndex := range system.Results {
			bound := binding
			system.Results[resultIndex].ExecutionAudit.Campaign = &bound
		}
		refreshPromotionResultsBinding(t, system)
	}
}

func addPromotionRepeatCampaignFixture(
	t *testing.T,
	input *PromotionBundleInput,
) int {
	t.Helper()
	taskCorpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
	require.NoError(t, err)
	candidateIndex := -1
	for index := range input.Systems {
		if input.Systems[index].Role == PromotionRoleCandidate {
			candidateIndex = index
			break
		}
	}
	require.NotEqual(t, -1, candidateIndex)
	configIndex := -1
	for index := range input.SystemManifest.Systems {
		if input.SystemManifest.Systems[index].SystemID ==
			input.Systems[candidateIndex].SystemID {
			configIndex = index
			break
		}
	}
	require.NotEqual(t, -1, configIndex)
	config := &input.SystemManifest.Systems[configIndex]
	config.Temperature = nil
	config.Seed = nil
	input.Systems[candidateIndex].Results = promotionContentResults(
		taskCorpus,
		*config,
		input.SystemManifestSHA256,
		input.Systems[candidateIndex].RouteSnapshotSHA256,
		1,
	)
	input.Systems[candidateIndex].AttemptEvidence = promotionAttemptFixture(
		t,
		taskCorpus,
		*config,
		input.Systems[candidateIndex].Results,
	)
	for finalistTaskIndex := range input.FinalistRoster.Tasks {
		stageOneCorpus := taskCorpus
		if input.FinalistRoster.Tasks[finalistTaskIndex].Task != input.Task {
			stageOneCorpus = finalistStageOneCorpusForTest(
				t,
				input.FinalistRoster.Tasks[finalistTaskIndex].Task,
			)
		}
		input.FinalistRoster.Tasks[finalistTaskIndex] =
			finalistTaskRosterBindManifestForTest(
				t,
				input.FinalistRoster.Tasks[finalistTaskIndex],
				stageOneCorpus,
				input.SystemManifest,
			)
	}
	rebuildPromotionCampaignFixture(t, input)

	sample := mustPromotionRepeatSample(t, *input)
	repeats := make([][]ResultRecord, PromotionRequiredRepeatRuns)
	for repeatIndex := range repeats {
		repeats[repeatIndex] = promotionContentResults(
			sample,
			*config,
			input.SystemManifestSHA256,
			input.Systems[candidateIndex].RouteSnapshotSHA256,
			1,
		)
		for resultIndex := range repeats[repeatIndex] {
			repeats[repeatIndex][resultIndex].Usage.RequestID = fmt.Sprintf(
				"candidate:repeat:%d:%04d",
				repeatIndex+1,
				resultIndex,
			)
		}
	}
	primaryPlanRaw, err := json.Marshal(input.CampaignPlan.Plan)
	require.NoError(t, err)
	var repeatPlan CampaignPlan
	require.NoError(t, json.Unmarshal(primaryPlanRaw, &repeatPlan))
	repeatPlan.CampaignID = "campaign-promotion-repeat-fixture"
	repeatPlan.Stage = CampaignStageRepeat
	repeatPlan.OutputRoot = "/var/tmp/bitagent-promotion-repeat-fixture"
	repeatPlan.SummaryPath = "repeat-summary.json"
	repeatPlan.TotalCostCapMicroUSD =
		int64(PromotionRequiredRepeatRuns*len(input.Systems)) * 100_000
	repeatPlan.TaskArtifacts[0].Corpus = CampaignArtifactBinding{
		ArtifactID: "promotion-content-repeat-sample",
		SHA256:     sample.SHA256,
	}
	repeatPlan.Runs = nil
	repeatPlan.Comparisons = nil
	for _, system := range input.Systems {
		for repeatIndex := 1; repeatIndex <= PromotionRequiredRepeatRuns; repeatIndex++ {
			repeatPlan.Runs = append(repeatPlan.Runs, CampaignRun{
				RunID: fmt.Sprintf(
					"promotion-repeat-%s-%d",
					system.SystemID,
					repeatIndex,
				),
				SystemID:        system.SystemID,
				Task:            input.Task,
				RepeatIndex:     repeatIndex,
				CostCapMicroUSD: 100_000,
				Outputs: CampaignRunOutputs{
					ResultsPath: fmt.Sprintf(
						"%s/repeat-%d-results.jsonl",
						system.SystemID,
						repeatIndex,
					),
					AttemptEvidencePath: fmt.Sprintf(
						"%s/repeat-%d-attempts.json",
						system.SystemID,
						repeatIndex,
					),
					ScorePath: fmt.Sprintf(
						"%s/repeat-%d-score.json",
						system.SystemID,
						repeatIndex,
					),
				},
			})
		}
	}
	for _, candidate := range input.Systems {
		if candidate.Role != PromotionRoleCandidate {
			continue
		}
		for _, control := range input.Systems {
			if control.Role != PromotionRoleControl {
				continue
			}
			for repeatIndex := 1; repeatIndex <= PromotionRequiredRepeatRuns; repeatIndex++ {
				repeatPlan.Comparisons = append(
					repeatPlan.Comparisons,
					CampaignComparison{
						ComparisonID: fmt.Sprintf(
							"promotion-repeat-comparison-%s-%s-%d",
							candidate.SystemID,
							control.SystemID,
							repeatIndex,
						),
						ControlRunID: fmt.Sprintf(
							"promotion-repeat-%s-%d",
							control.SystemID,
							repeatIndex,
						),
						CandidateRunID: fmt.Sprintf(
							"promotion-repeat-%s-%d",
							candidate.SystemID,
							repeatIndex,
						),
						OutputPath: fmt.Sprintf(
							"comparisons/%s-vs-%s-repeat-%d.json",
							candidate.SystemID,
							control.SystemID,
							repeatIndex,
						),
					},
				)
			}
		}
	}
	raw, err := json.Marshal(repeatPlan)
	require.NoError(t, err)
	repeatCampaign, err := ReadPromotionCampaignPlanEvidence(
		bytes.NewReader(raw),
	)
	require.NoError(t, err)
	input.RepeatCampaignPlan = &repeatCampaign

	evidence := &input.Systems[candidateIndex]
	evidence.RepeatResults = repeats
	evidence.RepeatArtifactSHA256 = []string{
		strings.Repeat("e", 64),
		strings.Repeat("f", 64),
	}
	for repeatIndex := range evidence.RepeatResults {
		binding, err := repeatCampaign.Plan.RunBinding(
			repeatCampaign.SHA256,
			fmt.Sprintf(
				"promotion-repeat-%s-%d",
				evidence.SystemID,
				repeatIndex+1,
			),
		)
		require.NoError(t, err)
		for resultIndex := range evidence.RepeatResults[repeatIndex] {
			bound := binding
			evidence.RepeatResults[repeatIndex][resultIndex].
				ExecutionAudit.Campaign = &bound
		}
		evidence.RepeatAttemptEvidence = append(
			evidence.RepeatAttemptEvidence,
			promotionAttemptFixture(
				t,
				sample,
				*config,
				evidence.RepeatResults[repeatIndex],
			),
		)
		evidence.RepeatAttemptEvidenceSHA256 = append(
			evidence.RepeatAttemptEvidenceSHA256,
			strings.Repeat(string(rune('6'+repeatIndex)), 64),
		)
	}
	return candidateIndex
}

func mustPromotionRepeatSample(
	t *testing.T,
	input PromotionBundleInput,
) Corpus {
	t.Helper()
	taskCorpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
	require.NoError(t, err)
	sample, _, err := promotionRepeatSample(
		taskCorpus,
		input.PromotionRoster.RepeatSampling,
	)
	require.NoError(t, err)
	return sample
}

func promotionFixtureConfig(
	t *testing.T,
	input PromotionBundleInput,
	systemID string,
) SystemConfig {
	t.Helper()
	for _, config := range input.SystemManifest.Systems {
		if config.SystemID == systemID {
			return config
		}
	}
	t.Fatalf("promotion fixture config %q not found", systemID)
	return SystemConfig{}
}

func TestBuildPromotionBundleRequiresCandidateToClearDeployedControl(t *testing.T) {
	input := promotionBundleFixture(t)
	normalizedControl := &input.Systems[0]
	candidate := &input.Systems[1]
	taskCorpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
	require.NoError(t, err)
	changed := 0
	for resultIndex, record := range taskCorpus.Records {
		if record.ContentFilter.Expected.Language != LanguageEnglish {
			continue
		}
		isEnglish := false
		for _, evidence := range []*PromotionSystemEvidence{
			normalizedControl,
			candidate,
		} {
			evidence.Results[resultIndex].ContentFilter.Action =
				ContentFilterActionDrop
			evidence.Results[resultIndex].ContentFilter.IsEnglish = &isEnglish
			resultsSHA, err := ResultsIdentity(evidence.Results)
			require.NoError(t, err)
			evidence.AttemptEvidence.ResultsSHA256 = resultsSHA
		}
		changed++
		if changed == 4 {
			break
		}
	}
	require.Equal(t, 4, changed)

	bundle, err := BuildPromotionBundle(input)
	require.NoError(t, err)
	report := requirePromotionSystemReport(t, bundle, "candidate")
	require.NotNil(t, report.Comparison)
	require.Equal(
		t,
		ComparisonPass,
		report.Comparison.Tasks[0].DiagnosticVerdict,
	)
	require.NotNil(t, report.ProductionComparison)
	require.Equal(
		t,
		ComparisonFail,
		report.ProductionComparison.Tasks[0].
			Gates.HarmNonInferiority.Decision,
	)
	require.False(t, report.CoreGatesPassed)
	require.False(t, report.Eligible)
	require.Empty(t, bundle.SelectedSystemID)
}

func TestBuildPromotionBundleRequiresFrozenProductionComparator(t *testing.T) {
	input := promotionBundleFixture(t)
	input.Systems[1].ProductionControlSystemID = ""
	input.PromotionRoster.Systems[1].ProductionControlSystemID = ""

	_, err := BuildPromotionBundle(input)
	require.ErrorContains(
		t,
		err,
		"candidate production_control_system_id is required",
	)
}

func TestBuildPromotionBundleRejectsPostHoldoutThresholdChange(t *testing.T) {
	input := promotionBundleFixture(t)
	input.Systems[1].DecisionThresholds.ContentDropConfidence = 0.70
	input.PromotionRoster.Systems[1].DecisionThresholds.
		ContentDropConfidence = 0.70
	taskIndex := taskIndexForFinalist(
		input.FinalistRoster,
		TaskContentFilter,
	)
	for index := range input.FinalistRoster.Tasks[taskIndex].SystemEvidence {
		evidence := &input.FinalistRoster.Tasks[taskIndex].SystemEvidence[index]
		if evidence.SystemID != input.Systems[1].SystemID {
			continue
		}
		evidence.DecisionThresholds = input.Systems[1].DecisionThresholds
		for _, comparison := range []*FinalistStageOneComparisonEvidence{
			evidence.NormalizedComparison,
			evidence.ProductionComparison,
		} {
			comparison.Report.Options.CandidateDecisionThresholds =
				evidence.DecisionThresholds.runtime()
			canonical, err := canonicalJSONIdentity(comparison.Report)
			require.NoError(t, err)
			comparison.CanonicalReportSHA256 = canonical
		}
		break
	}

	_, err := BuildPromotionBundle(input)
	require.ErrorContains(
		t,
		err,
		"request contract does not match the frozen decision thresholds",
	)
}

func TestBuildPromotionBundleRejectsActionBelowFrozenThreshold(t *testing.T) {
	input := promotionBundleFixture(t)
	candidate := &input.Systems[1]
	isEnglish := false
	candidate.Results[0].ContentFilter.Action = ContentFilterActionDrop
	candidate.Results[0].ContentFilter.IsEnglish = &isEnglish
	candidate.Results[0].ContentFilter.Confidence = 0.59
	resultsSHA, err := ResultsIdentity(candidate.Results)
	require.NoError(t, err)
	candidate.AttemptEvidence.ResultsSHA256 = resultsSHA

	_, err = BuildPromotionBundle(input)
	require.ErrorContains(
		t,
		err,
		"contentfilter.confidence: drop action requires at least 0.85",
	)
}

func TestPromotionV1KeepsSpecialistLaneDiagnosticOnly(t *testing.T) {
	require.False(t, promotionLaneEligible(EvaluationLaneSpecialist))
	require.True(t, promotionLaneEligible(EvaluationLaneNormalizedStrict))
	require.True(t, promotionLaneEligible(EvaluationLaneProductionFidelity))
}

func TestBuildPromotionBundleRejectsPackedRequestsWithoutPackedValidation(
	t *testing.T,
) {
	input := promotionBundleFixture(t)
	candidate := &input.Systems[1]
	first := candidate.AttemptEvidence.Attempts[0].Usage.RequestID
	// Duplicate a provider request ID only within the natural suite so Score's
	// packed-usage de-duplication remains structurally valid. Promotion must
	// still fail closed because v1 requires one independent request per case.
	candidate.AttemptEvidence.Attempts[1].Usage.RequestID = first
	candidate.Results[1].Usage.RequestID = first
	resultsSHA, err := ResultsIdentity(candidate.Results)
	require.NoError(t, err)
	candidate.AttemptEvidence.ResultsSHA256 = resultsSHA

	_, err = BuildPromotionBundle(input)
	require.ErrorContains(t, err, "primary request identity")
	require.ErrorContains(t, err, "was reused")
}

func TestBuildPromotionBundleFailsOnAttemptUsageTamper(t *testing.T) {
	input := promotionBundleFixture(t)
	input.Systems[1].AttemptEvidence.Attempts[0].Usage.CostMicroUSD++

	_, err := BuildPromotionBundle(input)
	require.ErrorContains(t, err, "differs from exact manifest estimate")
}

func TestPromotionV2RejectsForgedCostAmountAndProvenance(t *testing.T) {
	t.Run("positive amount cannot be edited consistently", func(t *testing.T) {
		input := promotionBundleFixture(t)
		candidate := &input.Systems[1]
		candidate.Results[0].Usage.CostMicroUSD++
		candidate.AttemptEvidence.Attempts[0].Usage.CostMicroUSD++
		refreshPromotionResultsBinding(t, candidate)

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "differs from exact manifest estimate")
	})

	t.Run("openrouter requires provider cost provenance", func(t *testing.T) {
		input := promotionBundleFixture(t)
		candidate := &input.Systems[1]
		candidate.Results[0].Usage.CostSource = CostSourceManifestEstimate
		candidate.AttemptEvidence.Attempts[0].Usage.CostSource =
			CostSourceManifestEstimate
		refreshPromotionResultsBinding(t, candidate)

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "incompatible with provider \"openrouter\"")
	})

	t.Run("direct openai requires manifest estimate provenance", func(t *testing.T) {
		input := promotionBundleFixture(t)
		control := &input.Systems[0]
		control.Results[0].Usage.CostSource = CostSourceProviderReported
		control.AttemptEvidence.Attempts[0].Usage.CostSource =
			CostSourceProviderReported
		refreshPromotionResultsBinding(t, control)

		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "incompatible with provider \"openai\"")
	})
}

func TestPromotionCostValidationUsesFrozenServiceTierPricing(t *testing.T) {
	config := promotionTestSystem("direct-tiered", "openai", 1)
	basis := PromotionCostBasis{
		OpenAIBatchMultiplierPPM: OpenAIBatchPricingPPM,
		OpenAIFlexMultiplierPPM:  400_000,
	}
	standard := promotionUsageForTest(config, "standard", 100, 10)

	batchConfig, err := promotionPricedSystem(
		config,
		PromotionServiceOpenAIBatch,
		basis,
	)
	require.NoError(t, err)
	batch := promotionUsageForTest(batchConfig, "batch", 100, 10)
	require.NoError(t, validatePromotionUsageCost(batchConfig, batch, "batch"))
	require.Less(t, batch.CostMicroUSD, standard.CostMicroUSD)
	require.ErrorContains(
		t,
		validatePromotionUsageCost(batchConfig, standard, "forged batch"),
		"differs from exact manifest estimate",
	)

	flexConfig, err := promotionPricedSystem(
		config,
		PromotionServiceOpenAIFlex,
		basis,
	)
	require.NoError(t, err)
	flex := promotionUsageForTest(flexConfig, "flex", 100, 10)
	require.NoError(t, validatePromotionUsageCost(flexConfig, flex, "flex"))
	require.Less(t, flex.CostMicroUSD, batch.CostMicroUSD)
}

func TestBuildPromotionBundleRequiresFullTaskHoldout(t *testing.T) {
	input := promotionBundleFixture(t)
	filtered, err := FilterCorpus(input.FullHoldout, TaskContentFilter, 100)
	require.NoError(t, err)
	input.FullHoldout = filtered

	_, err = BuildPromotionBundle(input)
	require.ErrorContains(t, err, "neither final development nor holdout")
}

func TestPromotionAttemptEvidenceOneRepairAndRuntimeFailurePolicy(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:repair", LanguageEnglish),
	})
	config := promotionTestSystem("candidate", "openrouter", 0.01)
	results := promotionContentResults(
		corpus,
		config,
		testManifestSHA,
		testRouteSnapshotSHA,
		10,
	)
	initial := promotionUsageForTest(config, "request:initial", 100, 2)
	repair := promotionUsageForTest(config, "request:repair", 110, 4)
	aggregate, err := sumPromotionUsage(initial, repair)
	require.NoError(t, err)
	results[0].Usage = aggregate
	resultsSHA, err := ResultsIdentity(results)
	require.NoError(t, err)
	evidence := PromotionAttemptEvidence{
		SchemaVersion:   SchemaVersion,
		ProtocolVersion: PromotionBundleProtocolVersion,
		SystemID:        config.SystemID,
		Task:            TaskContentFilter,
		CorpusSHA256:    corpus.SHA256,
		ResultsSHA256:   resultsSHA,
		Attempts: []PromotionAttempt{
			{
				CaseID:   corpus.Records[0].CaseID,
				Sequence: 1,
				Kind:     PromotionAttemptInitial,
				Status:   ResultStatusSchemaError,
				Usage:    initial,
			},
			{
				CaseID:   corpus.Records[0].CaseID,
				Sequence: 2,
				Kind:     PromotionAttemptSchemaRepair,
				Status:   ResultStatusOK,
				Usage:    repair,
			},
		},
	}
	report, err := validatePromotionAttemptEvidence(
		corpus,
		config,
		evidence,
		resultsSHA,
		results,
	)
	require.NoError(t, err)
	require.Equal(t, 1, report.SchemaRepairCases)
	require.Equal(t, 1.0, report.FinalValidity)
	require.False(t, report.Passed, "one failure in one case is below the 99.5%% floor")

	evidence.Attempts[0].Status = ResultStatusError
	_, err = validatePromotionAttemptEvidence(
		corpus,
		config,
		evidence,
		resultsSHA,
		results,
	)
	require.ErrorContains(t, err, "runtime failure cannot enter")
}

func TestPromotionAttemptEvidenceCountsFailedRepairAsCallError(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:failed-repair", LanguageEnglish),
	})
	config := promotionTestSystem("candidate", "openrouter", 0.01)
	results := promotionContentResults(
		corpus,
		config,
		testManifestSHA,
		testRouteSnapshotSHA,
		2,
	)
	initial := promotionUsageForTest(config, "failed-repair:initial", 40, 4)
	repair := promotionUsageForTest(config, "failed-repair:repair", 60, 6)
	aggregate, err := sumPromotionUsage(initial, repair)
	require.NoError(t, err)
	results[0].Status = ResultStatusError
	results[0].ErrorCode = "provider_error"
	results[0].ContentFilter = nil
	results[0].Usage = aggregate
	resultsSHA, err := ResultsIdentity(results)
	require.NoError(t, err)
	evidence := PromotionAttemptEvidence{
		SchemaVersion: SchemaVersion, ProtocolVersion: PromotionBundleProtocolVersion,
		SystemID: config.SystemID, Task: TaskContentFilter,
		CorpusSHA256: corpus.SHA256, ResultsSHA256: resultsSHA,
		Attempts: []PromotionAttempt{
			{
				CaseID: results[0].CaseID, Sequence: 1,
				Kind: PromotionAttemptInitial, Status: ResultStatusSchemaError,
				Usage: initial,
			},
			{
				CaseID: results[0].CaseID, Sequence: 2,
				Kind: PromotionAttemptSchemaRepair, Status: ResultStatusError,
				Usage: repair,
			},
		},
	}

	report, err := validatePromotionAttemptEvidence(
		corpus,
		config,
		evidence,
		resultsSHA,
		results,
	)
	require.NoError(t, err)
	require.Equal(t, 1, report.RuntimeFailureCases)
	require.Equal(t, 1, report.SchemaRepairCases)
	require.Equal(t, 1, report.MaximumRepairsObserved)
	require.Zero(t, report.Answered)
	require.InDelta(t, 1.0, report.CallErrorUpperConfidenceBound, 1e-12)
	require.False(t, report.Passed)
}

func TestPromotionAttemptEvidenceRejectsIncompleteUsageAccounting(t *testing.T) {
	corpus := mustCorpus(t, []CorpusRecord{
		testContentRecord("content:accounting", LanguageEnglish),
	})
	config := promotionTestSystem("candidate", "openrouter", 0.01)
	baselineResults := promotionContentResults(
		corpus,
		config,
		testManifestSHA,
		testRouteSnapshotSHA,
		1,
	)

	t.Run("result proof bit missing", func(t *testing.T) {
		results := append([]ResultRecord(nil), baselineResults...)
		results[0].Usage.AccountingComplete = false
		evidence := promotionAttemptFixture(t, corpus, config, results)
		_, err := validatePromotionAttemptEvidence(
			corpus,
			config,
			evidence,
			evidence.ResultsSHA256,
			results,
		)
		require.ErrorContains(t, err, "incomplete usage accounting")
	})

	t.Run("forged result proof lacks output dimension", func(t *testing.T) {
		results := append([]ResultRecord(nil), baselineResults...)
		results[0].Usage.OutputTokens = 0
		results[0].Usage.AccountingComplete = true
		evidence := promotionAttemptFixture(t, corpus, config, results)
		_, err := validatePromotionAttemptEvidence(
			corpus,
			config,
			evidence,
			evidence.ResultsSHA256,
			results,
		)
		require.ErrorContains(t, err, "incomplete usage accounting")
	})

	t.Run("attempt proof bit missing", func(t *testing.T) {
		results := append([]ResultRecord(nil), baselineResults...)
		evidence := promotionAttemptFixture(t, corpus, config, results)
		evidence.Attempts[0].Usage.AccountingComplete = false
		_, err := validatePromotionAttemptEvidence(
			corpus,
			config,
			evidence,
			evidence.ResultsSHA256,
			results,
		)
		require.ErrorContains(t, err, "usage accounting is incomplete")
	})

	t.Run("paid result cannot claim zero cost", func(t *testing.T) {
		results := append([]ResultRecord(nil), baselineResults...)
		results[0].Usage.CostMicroUSD = 0
		evidence := promotionAttemptFixture(t, corpus, config, results)
		_, err := validatePromotionAttemptEvidence(
			corpus,
			config,
			evidence,
			evidence.ResultsSHA256,
			results,
		)
		require.ErrorContains(t, err, "paid manifest route has zero usage cost")
	})

	t.Run("paid attempt cannot claim zero cost", func(t *testing.T) {
		results := append([]ResultRecord(nil), baselineResults...)
		evidence := promotionAttemptFixture(t, corpus, config, results)
		evidence.Attempts[0].Usage.CostMicroUSD = 0
		_, err := validatePromotionAttemptEvidence(
			corpus,
			config,
			evidence,
			evidence.ResultsSHA256,
			results,
		)
		require.ErrorContains(t, err, "paid manifest route has zero usage cost")
	})
}

func TestPromotionAttemptEvidencePassesAtLocked995FloorWithOneRepair(
	t *testing.T,
) {
	records := make([]CorpusRecord, 0, 200)
	for index := 0; index < 200; index++ {
		records = append(records, testContentRecord(
			fmt.Sprintf("content:repair-floor:%03d", index),
			LanguageEnglish,
		))
	}
	corpus := mustCorpus(t, records)
	config := promotionTestSystem("candidate", "openrouter", 0.01)
	results := promotionContentResults(
		corpus,
		config,
		testManifestSHA,
		testRouteSnapshotSHA,
		1,
	)
	initial := promotionUsageForTest(config, "repair-floor:initial", 40, 4)
	repair := promotionUsageForTest(config, "repair-floor:repair", 60, 6)
	aggregate, err := sumPromotionUsage(initial, repair)
	require.NoError(t, err)
	results[0].Usage = aggregate
	resultsSHA, err := ResultsIdentity(results)
	require.NoError(t, err)
	evidence := PromotionAttemptEvidence{
		SchemaVersion:   SchemaVersion,
		ProtocolVersion: PromotionBundleProtocolVersion,
		SystemID:        config.SystemID,
		Task:            TaskContentFilter,
		CorpusSHA256:    corpus.SHA256,
		ResultsSHA256:   resultsSHA,
	}
	for index, result := range results {
		if index == 0 {
			evidence.Attempts = append(evidence.Attempts,
				PromotionAttempt{
					CaseID:   result.CaseID,
					Sequence: 1,
					Kind:     PromotionAttemptInitial,
					Status:   ResultStatusSchemaError,
					Usage:    initial,
				},
				PromotionAttempt{
					CaseID:   result.CaseID,
					Sequence: 2,
					Kind:     PromotionAttemptSchemaRepair,
					Status:   ResultStatusOK,
					Usage:    repair,
				},
			)
			continue
		}
		evidence.Attempts = append(evidence.Attempts, PromotionAttempt{
			CaseID:   result.CaseID,
			Sequence: 1,
			Kind:     PromotionAttemptInitial,
			Status:   ResultStatusOK,
			Usage:    result.Usage,
		})
	}
	report, err := validatePromotionAttemptEvidence(
		corpus,
		config,
		evidence,
		resultsSHA,
		results,
	)
	require.NoError(t, err)
	require.InDelta(t, 0.995, report.FirstPassValidity, 1e-12)
	require.Equal(t, 1, report.SchemaRepairCases)
	require.Equal(t, 200, report.FinalValid)
	require.True(t, report.Passed)
}

func TestReadPromotionAttemptEvidenceIsStrictAndHashesExactBytes(t *testing.T) {
	evidence := PromotionAttemptEvidence{
		SchemaVersion:   SchemaVersion,
		ProtocolVersion: PromotionBundleProtocolVersion,
		SystemID:        "candidate",
		Task:            TaskContentFilter,
		CorpusSHA256:    strings.Repeat("a", 64),
		ResultsSHA256:   strings.Repeat("b", 64),
		Attempts: []PromotionAttempt{{
			CaseID:   "case:1",
			Sequence: 1,
			Kind:     PromotionAttemptInitial,
			Status:   ResultStatusOK,
			Usage: Usage{
				RequestID: "request:1",
			},
		}},
	}
	raw, err := json.Marshal(evidence)
	require.NoError(t, err)
	decoded, digest, err := ReadPromotionAttemptEvidence(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, evidence, decoded)
	require.Equal(t, sha256Hex(raw), digest)

	_, _, err = ReadPromotionAttemptEvidence(bytes.NewBufferString(
		`{"schema_version":1,"schema_version":1}`,
	))
	require.ErrorContains(t, err, "duplicate")
}

func TestPromotionBundleHolmFamilyContainsEverySuppliedCandidate(t *testing.T) {
	input := promotionBundleFixture(t)
	taskCorpus, err := FilterCorpus(input.FullHoldout, TaskContentFilter, 0)
	require.NoError(t, err)
	secondConfig := promotionTestSystem("candidate-second", "openrouter", 0.02)
	secondResults := promotionContentResults(
		taskCorpus,
		secondConfig,
		testManifestSHA,
		testRouteSnapshotSHA,
		2,
	)
	secondAttempts := promotionAttemptFixture(
		t,
		taskCorpus,
		secondConfig,
		secondResults,
	)
	input.SystemManifest.Systems = append(
		input.SystemManifest.Systems,
		secondConfig,
	)
	input.Systems = append(input.Systems, PromotionSystemEvidence{
		Role:                      PromotionRoleCandidate,
		SystemID:                  secondConfig.SystemID,
		ControlSystemID:           "control",
		ProductionControlSystemID: "production-control",
		DecisionThresholds: promotionDecisionThresholds(
			ProductionThresholds(),
		),
		DeploymentServiceTier: PromotionServiceStandard,
		Results:               secondResults,
		ResultsArtifactSHA256: strings.Repeat("2", 64),
		AttemptEvidence:       secondAttempts,
		AttemptEvidenceSHA256: strings.Repeat("1", 64),
		RouteSnapshotSHA256:   testRouteSnapshotSHA,
		RouteSnapshotVerified: true,
	})
	input.PromotionRoster.Systems = append(
		input.PromotionRoster.Systems,
		PromotionRosterSystem{
			Role:                      PromotionRoleCandidate,
			SystemID:                  secondConfig.SystemID,
			ControlSystemID:           "control",
			ProductionControlSystemID: "production-control",
			DecisionThresholds: promotionDecisionThresholds(
				ProductionThresholds(),
			),
			DeploymentServiceTier: PromotionServiceStandard,
		},
	)
	contentIndex := taskIndexForFinalist(
		input.FinalistRoster,
		TaskContentFilter,
	)
	input.FinalistRoster.Tasks[contentIndex] = finalistRosterAddCandidateForTest(
		t,
		input.FinalistRoster.Tasks[contentIndex],
		taskCorpus,
		secondConfig.SystemID,
	)
	input.FinalistRoster.Tasks[contentIndex] =
		finalistTaskRosterBindManifestForTest(
			t,
			input.FinalistRoster.Tasks[contentIndex],
			taskCorpus,
			input.SystemManifest,
		)
	rebuildPromotionCampaignFixture(t, &input)

	bundle, err := BuildPromotionBundle(input)
	require.NoError(t, err)
	require.True(t, bundle.ProtocolComplete)
	require.Equal(t, "candidate", bundle.SelectedSystemID)
	require.Len(t, bundle.HolmFamilies, 1)
	for _, family := range bundle.HolmFamilies {
		require.Len(t, family.Hypotheses, 2)
		require.Equal(
			t,
			"candidate",
			family.Hypotheses[0].CandidateSystemID,
		)
		require.Equal(
			t,
			"candidate-second",
			family.Hypotheses[1].CandidateSystemID,
		)
		require.True(t, family.Hypotheses[0].Passed)
		require.True(t, family.Hypotheses[1].Passed)
		require.Len(t, family.Hypotheses[0].Components, 6)
		require.Len(t, family.Hypotheses[1].Components, 6)
	}
}

func taskIndexForFinalist(
	manifest FinalistRosterManifest,
	task Task,
) int {
	for index, entry := range manifest.Tasks {
		if entry.Task == task {
			return index
		}
	}
	return -1
}

func TestPromotionBundleRequiresExactFrozenRoster(t *testing.T) {
	input := promotionBundleFixture(t)
	input.PromotionRoster.Systems = input.PromotionRoster.Systems[:1]

	_, err := BuildPromotionBundle(input)
	require.ErrorContains(
		t,
		err,
		"evidence does not exactly match the frozen roster",
	)
}

func TestPromotionBundleRequiresExactPreHoldoutFinalistRoster(t *testing.T) {
	input := promotionBundleFixture(t)
	index := taskIndexForFinalist(
		input.FinalistRoster,
		TaskContentFilter,
	)
	input.FinalistRoster.Tasks[index].PromotionRosterSHA256 =
		strings.Repeat("e", 64)

	_, err := BuildPromotionBundle(input)
	require.ErrorContains(
		t,
		err,
		"promotion roster does not match the exact task policy frozen before holdout review",
	)

	input = promotionBundleFixture(t)
	input.FinalistRosterSHA256 = strings.Repeat("e", 64)
	_, err = BuildPromotionBundle(input)
	require.ErrorContains(
		t,
		err,
		"does not match the exact manifest frozen before holdout review",
	)
}

func TestPromotionBundleBindsStageOneEvidenceToExactSystemManifest(
	t *testing.T,
) {
	mutateCandidate := func(
		t *testing.T,
		input *PromotionBundleInput,
		mutate func(*FinalistStageOneSystemEvidence),
	) {
		t.Helper()
		taskIndex := taskIndexForFinalist(
			input.FinalistRoster,
			TaskContentFilter,
		)
		require.NotEqual(t, -1, taskIndex)
		for index := range input.FinalistRoster.Tasks[taskIndex].SystemEvidence {
			evidence := &input.FinalistRoster.Tasks[taskIndex].SystemEvidence[index]
			if evidence.Role == PromotionRoleCandidate {
				mutate(evidence)
				return
			}
		}
		t.Fatal("candidate evidence not found")
	}

	t.Run("descriptor", func(t *testing.T) {
		input := promotionBundleFixture(t)
		mutateCandidate(t, &input, func(evidence *FinalistStageOneSystemEvidence) {
			evidence.Score.System.Model = "different/model"
			canonical, err := canonicalJSONIdentity(evidence.Score)
			require.NoError(t, err)
			evidence.CanonicalScoreReportSHA256 = canonical
		})
		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "descriptor does not match the exact system manifest")
	})

	t.Run("lane", func(t *testing.T) {
		input := promotionBundleFixture(t)
		mutateCandidate(t, &input, func(evidence *FinalistStageOneSystemEvidence) {
			evidence.Lane = EvaluationLaneProductionFidelity
		})
		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "lane does not match the exact system manifest")
	})

	t.Run("paid route cost", func(t *testing.T) {
		input := promotionBundleFixture(t)
		mutateCandidate(t, &input, func(evidence *FinalistStageOneSystemEvidence) {
			evidence.MeasuredStageOneCostMicroUSD = 0
			evidence.Score.Usage.CostMicroUSD = 0
			evidence.Score.Usage.CostUSD = 0
			for suiteIndex := range evidence.Score.Suites {
				suite := &evidence.Score.Suites[suiteIndex]
				suite.Usage.CostMicroUSD = 0
				suite.Usage.CostUSD = 0
				for taskIndex := range suite.Tasks {
					suite.Tasks[taskIndex].Usage.CostMicroUSD = 0
					suite.Tasks[taskIndex].Usage.CostUSD = 0
				}
			}
			canonical, err := canonicalJSONIdentity(evidence.Score)
			require.NoError(t, err)
			evidence.CanonicalScoreReportSHA256 = canonical
		})
		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "paid manifest route has zero usage cost")
	})

	t.Run("manifest-recomputed ranking cost", func(t *testing.T) {
		input := promotionBundleFixture(t)
		mutateCandidate(t, &input, func(evidence *FinalistStageOneSystemEvidence) {
			evidence.ManifestRecomputedStageOneCostMicroUSD++
		})
		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "manifest-recomputed Stage 1 cost")
	})

	t.Run("provider service tier", func(t *testing.T) {
		input := promotionBundleFixture(t)
		mutateCandidate(t, &input, func(evidence *FinalistStageOneSystemEvidence) {
			evidence.DeploymentServiceTier = PromotionServiceOpenAIBatch
		})
		_, err := BuildPromotionBundle(input)
		require.ErrorContains(t, err, "valid only for direct OpenAI systems")
	})
}

func TestReadFinalistRosterManifestIsStrictAndHashesExactBytes(t *testing.T) {
	input := promotionBundleFixture(t)
	raw, err := json.Marshal(input.FinalistRoster)
	require.NoError(t, err)

	decoded, digest, err := ReadFinalistRosterManifest(
		bytes.NewReader(raw),
		input.SystemManifest,
		input.SystemManifestSHA256,
	)
	require.NoError(t, err)
	require.Equal(t, input.FinalistRoster, decoded)
	require.Equal(t, sha256Hex(raw), digest)

	_, _, err = ReadFinalistRosterManifest(bytes.NewBufferString(
		`{"schema_version":1,"schema_version":1}`,
	), input.SystemManifest, input.SystemManifestSHA256)
	require.ErrorContains(t, err, "duplicate")

	var unknown map[string]any
	require.NoError(t, json.Unmarshal(raw, &unknown))
	unknown["unexpected"] = true
	unknownRaw, err := json.Marshal(unknown)
	require.NoError(t, err)
	_, _, err = ReadFinalistRosterManifest(
		bytes.NewReader(unknownRaw),
		input.SystemManifest,
		input.SystemManifestSHA256,
	)
	require.ErrorContains(t, err, "unknown field")
}

func TestFinalistRosterValidationRecomputesManifestRankingCost(t *testing.T) {
	input := promotionBundleFixture(t)
	tampered := input.FinalistRoster
	tamperedRaw, err := json.Marshal(tampered)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(tamperedRaw, &tampered))

	mutated := false
	for taskIndex := range tampered.Tasks {
		for evidenceIndex := range tampered.Tasks[taskIndex].SystemEvidence {
			evidence := &tampered.Tasks[taskIndex].SystemEvidence[evidenceIndex]
			if evidence.Role != PromotionRoleCandidate {
				continue
			}
			evidence.ManifestRecomputedStageOneCostMicroUSD++
			mutated = true
			break
		}
		if mutated {
			break
		}
	}
	require.True(t, mutated)

	err = tampered.Validate(
		input.SystemManifest,
		input.SystemManifestSHA256,
	)
	require.ErrorContains(t, err, "manifest-recomputed Stage 1 cost")

	err = input.FinalistRoster.Validate(
		input.SystemManifest,
		strings.Repeat("a", 64),
	)
	require.ErrorContains(
		t,
		err,
		"does not match the exact system manifest",
	)
}

func TestPromotionBundleAllowsOnlyOneControlPerLane(t *testing.T) {
	input := promotionBundleFixture(t)
	taskCorpus, err := FilterCorpus(input.FullHoldout, TaskContentFilter, 0)
	require.NoError(t, err)
	secondControl := promotionTestSystem("control-second", "openai", 1.0)
	secondResults := promotionContentResults(
		taskCorpus,
		secondControl,
		testManifestSHA,
		"",
		100,
	)
	input.SystemManifest.Systems = append(
		input.SystemManifest.Systems,
		secondControl,
	)
	input.Systems = append(input.Systems, PromotionSystemEvidence{
		Role:                  PromotionRoleControl,
		SystemID:              secondControl.SystemID,
		DeploymentServiceTier: PromotionServiceStandard,
		Results:               secondResults,
		ResultsArtifactSHA256: strings.Repeat("a", 64),
		AttemptEvidence: promotionAttemptFixture(
			t,
			taskCorpus,
			secondControl,
			secondResults,
		),
		AttemptEvidenceSHA256: strings.Repeat("b", 64),
	})
	input.PromotionRoster.Systems = append(
		input.PromotionRoster.Systems,
		PromotionRosterSystem{
			Role:                  PromotionRoleControl,
			SystemID:              secondControl.SystemID,
			DeploymentServiceTier: PromotionServiceStandard,
		},
	)
	stageOneControl := finalistSystemEvidenceForTest(
		t,
		taskCorpus,
		TaskContentFilter,
		secondControl.SystemID,
		secondControl.EvaluationLane,
		PromotionRoleControl,
	)
	taskIndex := taskIndexForFinalist(
		input.FinalistRoster,
		TaskContentFilter,
	)
	input.FinalistRoster.Tasks[taskIndex].SystemEvidence = append(
		input.FinalistRoster.Tasks[taskIndex].SystemEvidence,
		stageOneControl,
	)
	input.FinalistRoster.Tasks[taskIndex] =
		finalistTaskRosterBindManifestForTest(
			t,
			input.FinalistRoster.Tasks[taskIndex],
			taskCorpus,
			input.SystemManifest,
		)

	_, err = BuildPromotionBundle(input)
	require.ErrorContains(t, err, "requires exactly one normalized_strict")
}

func TestReadPromotionRosterIsStrictAndHashesExactBytes(t *testing.T) {
	raw := []byte(`{
	  "schema_version": 1,
	  "protocol_version": "bitagent-llm-promotion-v2",
	  "task": "contentfilter",
	  "system_manifest_sha256": "` + testManifestSHA + `",
	  "repeat_sampling": {
	    "algorithm_id": "sha256-task-suite-group-v1",
	    "natural_groups": 600,
	    "safety_groups": 600,
	    "confidence_level": 0.95,
	    "confidence_method": "wilson_score_one_sided",
	    "instability_ceiling": 0.01
	  },
		  "systems": [
		    {
		      "role": "control",
		      "system_id": "control",
		      "decision_thresholds": {"matcher_attach_confidence":0.6,"content_drop_confidence":0.85,"junk_confidence":0.8},
		      "deployment_service_tier": "standard"
		    },
		    {
		      "role": "control",
		      "system_id": "production-control",
		      "decision_thresholds": {"matcher_attach_confidence":0.6,"content_drop_confidence":0.85,"junk_confidence":0.8},
		      "deployment_service_tier": "openai_batch"
		    },
		    {
		      "role": "candidate",
		      "system_id": "candidate",
		      "control_system_id": "control",
		      "production_control_system_id": "production-control",
		      "decision_thresholds": {"matcher_attach_confidence":0.6,"content_drop_confidence":0.85,"junk_confidence":0.8},
		      "deployment_service_tier": "standard"
		    }
		  ]
	}
`)
	roster, digest, err := ReadPromotionRoster(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Len(t, roster.Systems, 3)
	require.Equal(t, sha256Hex(raw), digest)

	_, _, err = ReadPromotionRoster(bytes.NewBufferString(
		`{"schema_version":1,"schema_version":1}`,
	))
	require.ErrorContains(t, err, "duplicate")

	_, _, err = ReadPromotionRoster(bytes.NewBufferString(
		`{"schema_version":1,"surprise":true}`,
	))
	require.ErrorContains(t, err, "unknown field")
}

func TestPromotionInstabilityRequiredOnlyWithoutDeterministicControls(
	t *testing.T,
) {
	var records []CorpusRecord
	for index := 0; index <
		PromotionRepeatNaturalGroups+PromotionRepeatSafetyGroups; index++ {
		record := testContentRecord(
			fmt.Sprintf("content:repeat:%04d", index),
			LanguageEnglish,
		)
		record.GroupID = fmt.Sprintf("content:repeat-group:%04d", index)
		suite := "natural"
		if index >= PromotionRepeatNaturalGroups {
			suite = "safety"
		}
		record.SliceIDs = []string{"all", "fixture", "suite:" + suite}
		records = append(records, record)
	}
	corpus := mustCorpus(t, records)
	config := promotionTestSystem("direct-repeat", "openai", 1)
	config.Temperature = nil
	config.Seed = nil
	primary := promotionContentResults(corpus, config, testManifestSHA, "", 10)
	primaryAttempts := promotionAttemptFixture(
		t,
		corpus,
		config,
		primary,
	).Attempts
	repeatA := promotionContentResults(corpus, config, testManifestSHA, "", 10)
	repeatB := promotionContentResults(corpus, config, testManifestSHA, "", 10)
	for index := range repeatA {
		repeatA[index].Usage.RequestID = fmt.Sprintf("repeat:a:%04d", index)
		repeatB[index].Usage.RequestID = fmt.Sprintf("repeat:b:%04d", index)
	}
	repeatAttemptsFor := func(
		resultSets ...[]ResultRecord,
	) []PromotionAttemptEvidence {
		evidence := make([]PromotionAttemptEvidence, 0, len(resultSets))
		for _, results := range resultSets {
			evidence = append(
				evidence,
				promotionAttemptFixture(t, corpus, config, results),
			)
		}
		return evidence
	}
	repeatAttemptHashes := []string{
		strings.Repeat("c", 64),
		strings.Repeat("d", 64),
	}

	report, err := promotionInstability(
		corpus,
		config,
		primary,
		primaryAttempts,
		[][]ResultRecord{repeatA, repeatB},
		[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
		repeatAttemptsFor(repeatA, repeatB),
		repeatAttemptHashes,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.NoError(t, err)
	require.True(t, report.RepeatsRequired)
	require.Equal(t, 2, report.ObservedRepeatRuns)
	require.Zero(t, report.DivergentCases)
	require.Equal(t, 1200, report.Groups)
	require.Len(t, report.Suites, 2)
	require.Less(t, report.UpperConfidenceBound, report.Ceiling)
	require.True(t, report.Passed)

	validRepeatEvidence := repeatAttemptsFor(repeatA, repeatB)
	validRepeatHashes := []string{
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
	}
	for _, test := range []struct {
		name                  string
		repeatResults         [][]ResultRecord
		repeatResultHashes    []string
		repeatAttemptEvidence []PromotionAttemptEvidence
		repeatEvidenceHashes  []string
	}{
		{
			name:                  "missing result set",
			repeatResults:         [][]ResultRecord{repeatA},
			repeatResultHashes:    validRepeatHashes,
			repeatAttemptEvidence: validRepeatEvidence,
			repeatEvidenceHashes:  repeatAttemptHashes,
		},
		{
			name:                  "missing result hash",
			repeatResults:         [][]ResultRecord{repeatA, repeatB},
			repeatResultHashes:    validRepeatHashes[:1],
			repeatAttemptEvidence: validRepeatEvidence,
			repeatEvidenceHashes:  repeatAttemptHashes,
		},
		{
			name:                  "missing attempt evidence",
			repeatResults:         [][]ResultRecord{repeatA, repeatB},
			repeatResultHashes:    validRepeatHashes,
			repeatAttemptEvidence: validRepeatEvidence[:1],
			repeatEvidenceHashes:  repeatAttemptHashes,
		},
		{
			name:                  "missing attempt hash",
			repeatResults:         [][]ResultRecord{repeatA, repeatB},
			repeatResultHashes:    validRepeatHashes,
			repeatAttemptEvidence: validRepeatEvidence,
			repeatEvidenceHashes:  repeatAttemptHashes[:1],
		},
	} {
		t.Run("reject count mismatch "+test.name, func(t *testing.T) {
			_, err := promotionInstability(
				corpus,
				config,
				primary,
				primaryAttempts,
				test.repeatResults,
				test.repeatResultHashes,
				test.repeatAttemptEvidence,
				test.repeatEvidenceHashes,
				DefaultPromotionRepeatSamplingPlan(),
				testEvaluatorBuildSHA,
				testManifestSHA,
				ProductionThresholds(),
			)
			require.ErrorContains(t, err, "differ in count")
		})
	}

	invalidAttemptHashes := append([]string(nil), repeatAttemptHashes...)
	invalidAttemptHashes[1] = "not-a-sha256"
	_, err = promotionInstability(
		corpus,
		config,
		primary,
		primaryAttempts,
		[][]ResultRecord{repeatA, repeatB},
		validRepeatHashes,
		validRepeatEvidence,
		invalidAttemptHashes,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.ErrorContains(t, err, "repeat_attempt_evidence_sha256[1]")

	deterministicConfig := config
	temperature := 0.0
	seed := int64(20_260_727)
	deterministicConfig.Temperature = &temperature
	deterministicConfig.Seed = &seed
	duplicatePrimaryAttempts := append(
		[]PromotionAttempt(nil),
		primaryAttempts...,
	)
	duplicatePrimaryAttempts[1].Usage.RequestID =
		duplicatePrimaryAttempts[0].Usage.RequestID
	_, err = promotionInstability(
		corpus,
		deterministicConfig,
		primary,
		duplicatePrimaryAttempts,
		nil,
		nil,
		nil,
		nil,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.ErrorContains(t, err, "primary request identity")
	require.ErrorContains(t, err, "was reused")

	missingRepeat, err := promotionInstability(
		corpus,
		config,
		primary,
		primaryAttempts,
		[][]ResultRecord{repeatA},
		[]string{strings.Repeat("a", 64)},
		repeatAttemptsFor(repeatA),
		repeatAttemptHashes[:1],
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.NoError(t, err)
	require.False(t, missingRepeat.Passed)
	require.Equal(t, 1, missingRepeat.ObservedRepeatRuns)

	incompleteRepeatB := append([]ResultRecord(nil), repeatB...)
	incompleteRepeatB[0].Usage.AccountingComplete = false
	_, err = promotionInstability(
		corpus,
		config,
		primary,
		primaryAttempts,
		[][]ResultRecord{repeatA, incompleteRepeatB},
		[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
		repeatAttemptsFor(repeatA, incompleteRepeatB),
		repeatAttemptHashes,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.ErrorContains(t, err, "incomplete usage accounting")

	zeroCostRepeatB := append([]ResultRecord(nil), repeatB...)
	zeroCostRepeatB[0].Usage.CostMicroUSD = 0
	_, err = promotionInstability(
		corpus,
		config,
		primary,
		primaryAttempts,
		[][]ResultRecord{repeatA, zeroCostRepeatB},
		[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
		repeatAttemptsFor(repeatA, zeroCostRepeatB),
		repeatAttemptHashes,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.ErrorContains(t, err, "paid manifest route has zero usage cost")

	_, err = promotionInstability(
		corpus,
		config,
		primary,
		primaryAttempts,
		[][]ResultRecord{repeatA[:len(repeatA)-1], repeatB},
		[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
		repeatAttemptsFor(repeatA[:len(repeatA)-1], repeatB),
		repeatAttemptHashes,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.ErrorContains(t, err, "does not cover every sampled case")

	wrongContractRepeatB := append([]ResultRecord(nil), repeatB...)
	wrongContractRepeatB[0].RequestContractSHA256 = strings.Repeat("f", 64)
	_, err = promotionInstability(
		corpus,
		config,
		primary,
		primaryAttempts,
		[][]ResultRecord{repeatA, wrongContractRepeatB},
		[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
		repeatAttemptsFor(repeatA, wrongContractRepeatB),
		repeatAttemptHashes,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.ErrorContains(t, err, "request contract differs")

	primaryInitialID := "primary:repair:initial"
	primaryRepairID := "primary:repair:retry"
	initialUsage := promotionUsageForTest(config, primaryInitialID, 40, 2)
	repairUsage := promotionUsageForTest(config, primaryRepairID, 60, 8)
	aggregateUsage, err := sumPromotionUsage(initialUsage, repairUsage)
	require.NoError(t, err)
	repairedPrimary := append([]ResultRecord(nil), primary...)
	repairedPrimary[0].Usage = aggregateUsage
	repairedAttempts := append([]PromotionAttempt(nil), primaryAttempts...)
	repairedAttempts[0] = PromotionAttempt{
		CaseID:   repairedPrimary[0].CaseID,
		Sequence: 1,
		Kind:     PromotionAttemptInitial,
		Status:   ResultStatusSchemaError,
		Usage:    initialUsage,
	}
	repairedAttempts = append(repairedAttempts, PromotionAttempt{
		CaseID:   repairedPrimary[0].CaseID,
		Sequence: 2,
		Kind:     PromotionAttemptSchemaRepair,
		Status:   ResultStatusOK,
		Usage:    repairUsage,
	})
	for _, reusedID := range []string{primaryInitialID, primaryRepairID} {
		t.Run("reject reused "+reusedID, func(t *testing.T) {
			reusedRepeatA := append([]ResultRecord(nil), repeatA...)
			reusedRepeatA[0].Usage.RequestID = reusedID
			_, err := promotionInstability(
				corpus,
				config,
				repairedPrimary,
				repairedAttempts,
				[][]ResultRecord{reusedRepeatA, repeatB},
				[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
				repeatAttemptsFor(reusedRepeatA, repeatB),
				repeatAttemptHashes,
				DefaultPromotionRepeatSamplingPlan(),
				testEvaluatorBuildSHA,
				testManifestSHA,
				ProductionThresholds(),
			)
			require.ErrorContains(t, err, "was reused")
		})
	}

	buildRepairedRepeat := func(
		initialID string,
		repairID string,
	) ([]ResultRecord, PromotionAttemptEvidence) {
		repaired := append([]ResultRecord(nil), repeatA...)
		initial := promotionUsageForTest(config, initialID, 40, 2)
		repair := promotionUsageForTest(config, repairID, 60, 8)
		aggregate, aggregateErr := sumPromotionUsage(initial, repair)
		require.NoError(t, aggregateErr)
		repaired[0].Usage = aggregate
		evidence := promotionAttemptFixture(t, corpus, config, repaired)
		evidence.Attempts[0] = PromotionAttempt{
			CaseID:   repaired[0].CaseID,
			Sequence: 1,
			Kind:     PromotionAttemptInitial,
			Status:   ResultStatusSchemaError,
			Usage:    initial,
		}
		evidence.Attempts = append(evidence.Attempts, PromotionAttempt{
			CaseID:   repaired[0].CaseID,
			Sequence: 2,
			Kind:     PromotionAttemptSchemaRepair,
			Status:   ResultStatusOK,
			Usage:    repair,
		})
		return repaired, evidence
	}

	t.Run("repeat repair cannot reuse primary attempt", func(t *testing.T) {
		repairedRepeatA, repairedRepeatAttempts := buildRepairedRepeat(
			primaryAttempts[0].Usage.RequestID,
			"repeat:repair:unique",
		)
		_, err := promotionInstability(
			corpus,
			config,
			primary,
			primaryAttempts,
			[][]ResultRecord{repairedRepeatA, repeatB},
			[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
			[]PromotionAttemptEvidence{
				repairedRepeatAttempts,
				promotionAttemptFixture(t, corpus, config, repeatB),
			},
			repeatAttemptHashes,
			DefaultPromotionRepeatSamplingPlan(),
			testEvaluatorBuildSHA,
			testManifestSHA,
			ProductionThresholds(),
		)
		require.ErrorContains(t, err, "was reused")
	})

	t.Run("other repeat cannot reuse repair attempt", func(t *testing.T) {
		const repairID = "repeat:repair:charged"
		repairedRepeatA, repairedRepeatAttempts := buildRepairedRepeat(
			"repeat:repair:initial",
			repairID,
		)
		duplicateRepeatB := append([]ResultRecord(nil), repeatB...)
		duplicateRepeatB[0].Usage.RequestID = repairID
		_, err := promotionInstability(
			corpus,
			config,
			primary,
			primaryAttempts,
			[][]ResultRecord{repairedRepeatA, duplicateRepeatB},
			[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
			[]PromotionAttemptEvidence{
				repairedRepeatAttempts,
				promotionAttemptFixture(t, corpus, config, duplicateRepeatB),
			},
			repeatAttemptHashes,
			DefaultPromotionRepeatSamplingPlan(),
			testEvaluatorBuildSHA,
			testManifestSHA,
			ProductionThresholds(),
		)
		require.ErrorContains(t, err, "was reused")
	})

	for index := range repeatB {
		repeatB[index].ExecutionAudit.Route.ReturnedModel = "other/model"
	}
	_, err = promotionInstability(
		corpus,
		config,
		primary,
		primaryAttempts,
		[][]ResultRecord{repeatA, repeatB},
		[]string{strings.Repeat("a", 64), strings.Repeat("b", 64)},
		repeatAttemptsFor(repeatA, repeatB),
		repeatAttemptHashes,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.ErrorContains(t, err, "route binding differs")
}

func TestPromotionInstabilityBindsRepeatAttemptsToDeterministicSample(
	t *testing.T,
) {
	const groupsPerSuite = 700
	var records []CorpusRecord
	for index := 0; index < groupsPerSuite*2; index++ {
		record := testContentRecord(
			fmt.Sprintf("content:large-repeat:%04d", index),
			LanguageEnglish,
		)
		record.GroupID = fmt.Sprintf("content:large-repeat-group:%04d", index)
		suite := "natural"
		if index >= groupsPerSuite {
			suite = "safety"
		}
		record.SliceIDs = []string{"all", "fixture", "suite:" + suite}
		records = append(records, record)
	}
	corpus := mustCorpus(t, records)
	sample, _, err := promotionRepeatSample(
		corpus,
		DefaultPromotionRepeatSamplingPlan(),
	)
	require.NoError(t, err)
	require.Len(t, corpus.Records, groupsPerSuite*2)
	require.Len(
		t,
		sample.Records,
		PromotionRepeatNaturalGroups+PromotionRepeatSafetyGroups,
	)
	require.NotEqual(t, corpus.SHA256, sample.SHA256)

	config := promotionTestSystem("direct-repeat-large", "openai", 1)
	config.Temperature = nil
	config.Seed = nil
	primary := promotionContentResults(corpus, config, testManifestSHA, "", 10)
	primaryEvidence := promotionAttemptFixture(t, corpus, config, primary)
	repeatA := promotionContentResults(sample, config, testManifestSHA, "", 10)
	repeatB := promotionContentResults(sample, config, testManifestSHA, "", 10)
	for index := range repeatA {
		repeatA[index].Usage.RequestID = fmt.Sprintf("repeat:large:a:%04d", index)
		repeatB[index].Usage.RequestID = fmt.Sprintf("repeat:large:b:%04d", index)
	}
	repeatEvidence := []PromotionAttemptEvidence{
		promotionAttemptFixture(t, sample, config, repeatA),
		promotionAttemptFixture(t, sample, config, repeatB),
	}
	repeatResultHashes := []string{
		strings.Repeat("3", 64),
		strings.Repeat("4", 64),
	}
	repeatEvidenceHashes := []string{
		strings.Repeat("5", 64),
		strings.Repeat("6", 64),
	}

	instability, err := promotionInstability(
		corpus,
		config,
		primary,
		primaryEvidence.Attempts,
		[][]ResultRecord{repeatA, repeatB},
		repeatResultHashes,
		repeatEvidence,
		repeatEvidenceHashes,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.NoError(t, err)
	require.True(t, instability.Passed)
	require.Equal(t, len(sample.Records), instability.Cases)

	wrongCorpusEvidence := append(
		[]PromotionAttemptEvidence(nil),
		repeatEvidence...,
	)
	wrongCorpusEvidence[0] = promotionAttemptFixture(
		t,
		corpus,
		config,
		repeatA,
	)
	_, err = promotionInstability(
		corpus,
		config,
		primary,
		primaryEvidence.Attempts,
		[][]ResultRecord{repeatA, repeatB},
		repeatResultHashes,
		wrongCorpusEvidence,
		repeatEvidenceHashes,
		DefaultPromotionRepeatSamplingPlan(),
		testEvaluatorBuildSHA,
		testManifestSHA,
		ProductionThresholds(),
	)
	require.ErrorContains(t, err, "attempt evidence binding")

	report, err := buildPromotionSystemReport(
		corpus,
		config,
		PromotionSystemEvidence{
			Role:                        PromotionRoleControl,
			SystemID:                    config.SystemID,
			DecisionThresholds:          promotionDecisionThresholds(ProductionThresholds()),
			DeploymentServiceTier:       PromotionServiceStandard,
			Results:                     primary,
			ResultsArtifactSHA256:       strings.Repeat("1", 64),
			AttemptEvidence:             primaryEvidence,
			AttemptEvidenceSHA256:       strings.Repeat("2", 64),
			RepeatResults:               [][]ResultRecord{repeatA, repeatB},
			RepeatArtifactSHA256:        repeatResultHashes,
			RepeatAttemptEvidence:       repeatEvidence,
			RepeatAttemptEvidenceSHA256: repeatEvidenceHashes,
		},
		DefaultPromotionRepeatSamplingPlan(),
		testManifestSHA,
		testEvaluatorBuildSHA,
		PromotionCostBasis{
			ObservedWindowStartUTC:   "2026-07-17T00:00:00Z",
			ObservedWindowEndUTC:     "2026-07-24T00:00:00Z",
			ObservedEligibleRequests: int64(len(primary)),
			VolumeEvidenceSHA256:     strings.Repeat("7", 64),
			PricingEvidenceSHA256:    strings.Repeat("8", 64),
			OpenAIBatchMultiplierPPM: OpenAIBatchPricingPPM,
			OpenAIFlexMultiplierPPM:  500_000,
		},
		7,
	)
	require.NoError(t, err)
	require.Equal(
		t,
		repeatEvidenceHashes,
		report.RepeatAttemptEvidenceSHA256,
	)
}

func TestPromotionRepeatSampleUsesOneStableRepresentativePerGroup(
	t *testing.T,
) {
	t.Parallel()
	var records []CorpusRecord
	for index := 0; index <
		PromotionRepeatNaturalGroups+PromotionRepeatSafetyGroups; index++ {
		suite := "natural"
		if index >= PromotionRepeatNaturalGroups {
			suite = "safety"
		}
		groupID := fmt.Sprintf("repeat-family:%04d", index)
		for _, suffix := range []string{"z", "a"} {
			record := testContentRecord(
				fmt.Sprintf("repeat-case:%04d:%s", index, suffix),
				LanguageEnglish,
			)
			record.GroupID = groupID
			record.SliceIDs = []string{
				"all",
				"fixture",
				"suite:" + suite,
			}
			records = append(records, record)
		}
	}
	corpus := mustCorpus(t, records)

	sample, suites, err := promotionRepeatSample(
		corpus,
		DefaultPromotionRepeatSamplingPlan(),
	)
	require.NoError(t, err)
	require.Len(
		t,
		sample.Records,
		PromotionRepeatNaturalGroups+PromotionRepeatSafetyGroups,
	)
	require.Len(t, suites, 2)
	for _, suite := range suites {
		require.Equal(t, suite.SampledGroups, suite.SampledCases)
	}
	seenGroups := make(map[string]struct{}, len(sample.Records))
	for _, record := range sample.Records {
		require.NotContains(t, seenGroups, record.GroupID)
		seenGroups[record.GroupID] = struct{}{}
		require.True(
			t,
			strings.HasSuffix(record.CaseID, ":a"),
			"representative case_id = %q",
			record.CaseID,
		)
	}
}

func promotionBundleFixture(t *testing.T) PromotionBundleInput {
	t.Helper()
	// The campaign's 0.1% one-sided harm ceiling needs at least 2,704
	// harm-eligible observations even when no harmful event is observed. Keep
	// both language classes represented so balanced accuracy is measurable.
	const naturalCases = 2000
	const safetyCases = 2000
	var records []CorpusRecord
	records = append(records,
		testExtractRecord("extract:closure", MatcherExtractExpected{
			Acceptable: []MatcherExtraction{testExtraction("Example Movie")},
		}),
		testRerankRecord("rerank:closure", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101},
		}),
		testJunkRecord("junk:closure", JunkClassDegenerate),
	)
	for index := 0; index < naturalCases+safetyCases; index++ {
		language := LanguageEnglish
		if index%4 == 0 {
			language = LanguageNonEnglish
		}
		record := testContentRecord(
			fmt.Sprintf("content:%04d", index),
			language,
		)
		record.GroupID = fmt.Sprintf("content-group:%04d", index)
		suite := "natural"
		if index >= naturalCases {
			suite = "safety"
		}
		record.SliceIDs = []string{"all", "fixture", "suite:" + suite}
		if language == LanguageEnglish {
			record.SliceIDs = append(
				record.SliceIDs,
				"hard_must_keep_english",
			)
		} else {
			record.SliceIDs = append(
				record.SliceIDs,
				"latin_script_non_english",
			)
		}
		records = append(records, record)
	}
	full := mustCorpus(t, records)
	closure := promotionGoldClosureManifest(full)
	corpusPlan, corpusPlanBytes, corpusPlanSHA := promotionCorpusPlanFixture(t)
	closure.PlanID = corpusPlan.PlanID
	closure.PlanSHA256 = corpusPlanSHA
	taskCorpus, err := FilterCorpus(full, TaskContentFilter, 0)
	require.NoError(t, err)

	controlConfig := promotionTestSystem("control", "openai", 1.0)
	controlConfig.Tasks = append([]Task(nil), orderedTasks...)
	productionControlConfig := promotionTestSystem(
		"production-control",
		"openai",
		1.0,
	)
	productionControlConfig.EvaluationLane =
		EvaluationLaneProductionFidelity
	productionControlConfig.Tasks = append([]Task(nil), orderedTasks...)
	productionControlConfig.OutputContract = OutputContractPromptOnly
	productionControlConfig.StructuredOutputs = false
	candidateConfig := promotionTestSystem("candidate", "openrouter", 0.01)
	candidateConfig.Tasks = append([]Task(nil), orderedTasks...)
	manifest := SystemManifest{
		SchemaVersion: SchemaVersion,
		Systems: []SystemConfig{
			controlConfig,
			productionControlConfig,
			candidateConfig,
		},
	}
	require.NoError(t, manifest.Validate())

	controlResults := promotionContentResults(
		taskCorpus,
		controlConfig,
		testManifestSHA,
		"",
		100,
	)
	candidateResults := promotionContentResults(
		taskCorpus,
		candidateConfig,
		testManifestSHA,
		testRouteSnapshotSHA,
		1,
	)
	productionControlResults := promotionContentResults(
		taskCorpus,
		productionControlConfig,
		testManifestSHA,
		"",
		100,
	)
	controlEvidence := promotionAttemptFixture(
		t,
		taskCorpus,
		controlConfig,
		controlResults,
	)
	candidateEvidence := promotionAttemptFixture(
		t,
		taskCorpus,
		candidateConfig,
		candidateResults,
	)
	productionControlEvidence := promotionAttemptFixture(
		t,
		taskCorpus,
		productionControlConfig,
		productionControlResults,
	)
	input := PromotionBundleInput{
		Task:                      TaskContentFilter,
		FullHoldout:               full,
		CorpusPlanBytes:           corpusPlanBytes,
		CorpusPlanSHA256:          corpusPlanSHA,
		GoldClosureManifest:       closure,
		GoldClosureManifestSHA256: strings.Repeat("9", 64),
		FinalistRosterSHA256:      closure.FinalistRosterManifestSHA256,
		SystemManifest:            manifest,
		SystemManifestSHA256:      testManifestSHA,
		PromotionRosterSHA256:     strings.Repeat("0", 64),
		EvaluatorBuildSHA256:      testEvaluatorBuildSHA,
		CostBasis: PromotionCostBasis{
			ObservedWindowStartUTC:   "2026-07-17T00:00:00Z",
			ObservedWindowEndUTC:     "2026-07-24T00:00:00Z",
			ObservedEligibleRequests: 14_000,
			VolumeEvidenceSHA256:     strings.Repeat("8", 64),
			PricingEvidenceSHA256:    strings.Repeat("7", 64),
			OpenAIBatchMultiplierPPM: OpenAIBatchPricingPPM,
			OpenAIFlexMultiplierPPM:  500_000,
		},
		Systems: []PromotionSystemEvidence{
			{
				Role:     PromotionRoleControl,
				SystemID: controlConfig.SystemID,
				DecisionThresholds: promotionDecisionThresholds(
					ProductionThresholds(),
				),
				DeploymentServiceTier: PromotionServiceStandard,
				Results:               controlResults,
				ResultsArtifactSHA256: strings.Repeat("6", 64),
				AttemptEvidence:       controlEvidence,
				AttemptEvidenceSHA256: strings.Repeat("5", 64),
			},
			{
				Role:                      PromotionRoleCandidate,
				SystemID:                  candidateConfig.SystemID,
				ControlSystemID:           controlConfig.SystemID,
				ProductionControlSystemID: productionControlConfig.SystemID,
				DecisionThresholds: promotionDecisionThresholds(
					ProductionThresholds(),
				),
				DeploymentServiceTier: PromotionServiceStandard,
				Results:               candidateResults,
				ResultsArtifactSHA256: strings.Repeat("4", 64),
				AttemptEvidence:       candidateEvidence,
				AttemptEvidenceSHA256: strings.Repeat("3", 64),
				RouteSnapshotSHA256:   testRouteSnapshotSHA,
				RouteSnapshotVerified: true,
			},
			{
				Role:     PromotionRoleControl,
				SystemID: productionControlConfig.SystemID,
				DecisionThresholds: promotionDecisionThresholds(
					ProductionThresholds(),
				),
				DeploymentServiceTier: PromotionServiceStandard,
				Results:               productionControlResults,
				ResultsArtifactSHA256: strings.Repeat("2", 64),
				AttemptEvidence:       productionControlEvidence,
				AttemptEvidenceSHA256: strings.Repeat("1", 64),
			},
		},
		ComparisonOptions: ComparisonOptions{
			BootstrapReplicates: 2_000,
			BootstrapSeed:       20_260_724,
		},
	}
	input.PromotionRoster = PromotionRoster{
		SchemaVersion:        SchemaVersion,
		ProtocolVersion:      PromotionBundleProtocolVersion,
		Task:                 input.Task,
		SystemManifestSHA256: input.SystemManifestSHA256,
		RepeatSampling:       DefaultPromotionRepeatSamplingPlan(),
	}
	for _, evidence := range input.Systems {
		input.PromotionRoster.Systems = append(
			input.PromotionRoster.Systems,
			PromotionRosterSystem{
				Role:                      evidence.Role,
				SystemID:                  evidence.SystemID,
				ControlSystemID:           evidence.ControlSystemID,
				ProductionControlSystemID: evidence.ProductionControlSystemID,
				DecisionThresholds:        evidence.DecisionThresholds,
				DeploymentServiceTier:     evidence.DeploymentServiceTier,
			},
		)
	}
	input.FinalistRoster = FinalistRosterManifest{
		SchemaVersion:                    SchemaVersion,
		ProtocolVersion:                  FinalistRosterProtocolVersion,
		PlanSHA256:                       closure.PlanSHA256,
		DevelopmentClosureManifestSHA256: closure.DevelopmentClosureManifestSHA256,
		DevelopmentCorpusSHA256:          closure.DevelopmentCorpusSHA256,
		SystemManifestSHA256:             input.SystemManifestSHA256,
		Ordering:                         FinalistRosterOrdering,
	}
	finalistSystems := finalistRosterSystemIDs{
		normalized: controlConfig.SystemID,
		production: productionControlConfig.SystemID,
		candidate:  candidateConfig.SystemID,
	}
	for _, task := range orderedTasks {
		// The bundle's own task binds the real frozen corpus; the other three
		// only have to carry structurally valid Stage 1 evidence, because the
		// per-task corpus recomputation lives in FinalizeHoldoutGold.
		stageOneCorpus := taskCorpus
		if task != TaskContentFilter {
			stageOneCorpus = finalistStageOneCorpusForTest(t, task)
		}
		roster := finalistTaskRosterFromCorpus(
			t,
			stageOneCorpus,
			task,
			finalistSystems,
			strings.Repeat("d", 64),
			input.PromotionRosterSHA256,
		)
		roster = finalistTaskRosterBindManifestForTest(
			t,
			roster,
			stageOneCorpus,
			input.SystemManifest,
		)
		input.FinalistRoster.Tasks = append(
			input.FinalistRoster.Tasks,
			roster,
		)
	}
	rebuildPromotionCampaignFixture(t, &input)
	return input
}

func promotionCorpusPlanFixture(t *testing.T) (CorpusPlan, []byte, string) {
	t.Helper()
	plan, _ := readCheckedInCampaignCorpusPlan(t)
	plan.PlanID = "plan:fixture"
	plan.Executable = true
	plan.NonExecutableReason = ""
	for index := range plan.Tasks {
		task := &plan.Tasks[index]
		switch task.Task {
		case TaskMatcherExtract:
			task.RequiredSafetySlices = []string{"must_extract"}
		case TaskMatcherRerank:
			task.RequiredSafetySlices = []string{"fixture"}
		case TaskContentFilter:
			task.RequiredSafetyCounts = map[string]int{
				"hard_must_keep_english":   2_000,
				"latin_script_non_english": 500,
			}
		}
	}
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	exact, digest, err := ReadCorpusPlan(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, plan.PlanID, exact.PlanID)
	return exact, raw, digest
}

func promotionMatcherExtractBundleFixture(t *testing.T) PromotionBundleInput {
	t.Helper()
	const naturalCases = 2000
	const safetyCases = 2000
	records := []CorpusRecord{
		testRerankRecord("rerank:closure", MatcherRerankExpected{
			AcceptableTMDBIDs: []int64{101},
		}),
		testContentRecord("content:closure", LanguageEnglish),
		testJunkRecord("junk:closure", JunkClassDegenerate),
	}
	for index := 0; index < naturalCases+safetyCases; index++ {
		record := testExtractRecord(
			fmt.Sprintf("extract:promotion:%04d", index),
			MatcherExtractExpected{
				Acceptable: []MatcherExtraction{
					testExtraction("Example Movie"),
				},
			},
		)
		record.GroupID = fmt.Sprintf("extract-promotion-group:%04d", index)
		suite := "natural"
		if index >= naturalCases {
			suite = "safety"
		}
		record.SliceIDs = []string{
			"all", "fixture", "must_extract", "suite:" + suite,
		}
		records = append(records, record)
	}
	full := mustCorpus(t, records)
	closure := promotionGoldClosureManifest(full)
	taskCorpus, err := FilterCorpus(full, TaskMatcherExtract, 0)
	require.NoError(t, err)
	require.Len(t, taskCorpus.Records, naturalCases+safetyCases)

	// Reuse the fully bound three-system fixture shell, then replace every
	// task-dependent artifact before building the matcher campaign seal.
	input := promotionBundleFixture(t)
	closure.PlanID = input.GoldClosureManifest.PlanID
	closure.PlanSHA256 = input.CorpusPlanSHA256
	input.Task = TaskMatcherExtract
	input.FullHoldout = full
	input.GoldClosureManifest = closure
	input.FinalistRosterSHA256 = closure.FinalistRosterManifestSHA256
	for index := range input.SystemManifest.Systems {
		input.SystemManifest.Systems[index].RequestTimeoutMS =
			CampaignTaskDeadlineMS(TaskMatcherExtract)
	}
	require.NoError(t, input.SystemManifest.Validate())
	for index := range input.Systems {
		system := &input.Systems[index]
		config := promotionFixtureConfig(t, input, system.SystemID)
		cost := int64(100)
		if system.Role == PromotionRoleCandidate {
			cost = 1
		}
		system.Results = promotionMatcherExtractResults(
			t,
			taskCorpus,
			config,
			input.SystemManifestSHA256,
			system.RouteSnapshotSHA256,
			cost,
		)
		system.AttemptEvidence = promotionAttemptFixture(
			t,
			taskCorpus,
			config,
			system.Results,
		)
	}
	input.PromotionRoster.Task = input.Task
	input.FinalistRoster = FinalistRosterManifest{
		SchemaVersion:                    SchemaVersion,
		ProtocolVersion:                  FinalistRosterProtocolVersion,
		PlanSHA256:                       closure.PlanSHA256,
		DevelopmentClosureManifestSHA256: closure.DevelopmentClosureManifestSHA256,
		DevelopmentCorpusSHA256:          closure.DevelopmentCorpusSHA256,
		SystemManifestSHA256:             input.SystemManifestSHA256,
		Ordering:                         FinalistRosterOrdering,
	}
	finalistSystems := finalistRosterSystemIDs{
		normalized: "control",
		production: "production-control",
		candidate:  "candidate",
	}
	for _, task := range orderedTasks {
		stageOneCorpus := taskCorpus
		if task != input.Task {
			stageOneCorpus = finalistStageOneCorpusForTest(t, task)
		}
		roster := finalistTaskRosterFromCorpus(
			t,
			stageOneCorpus,
			task,
			finalistSystems,
			strings.Repeat("d", 64),
			input.PromotionRosterSHA256,
		)
		roster = finalistTaskRosterBindManifestForTest(
			t,
			roster,
			stageOneCorpus,
			input.SystemManifest,
		)
		input.FinalistRoster.Tasks = append(input.FinalistRoster.Tasks, roster)
	}
	rebuildPromotionCampaignFixture(t, &input)
	return input
}

func promotionTestSystem(
	systemID string,
	provider string,
	inputPrice float64,
) SystemConfig {
	temperature := 0.0
	seed := int64(20_260_724)
	config := SystemConfig{
		SystemID:                 systemID,
		Provider:                 provider,
		Model:                    "test/model",
		Variant:                  "test-route",
		PromptVersion:            "eval-v1",
		EvaluationLane:           EvaluationLaneNormalizedStrict,
		APIKind:                  APIKindChat,
		OutputContract:           OutputContractJSONSchema,
		APIKeyEnv:                "TEST_API_KEY",
		Tasks:                    []Task{TaskContentFilter},
		InputUSDPerMillion:       inputPrice,
		CachedInputUSDPerMillion: inputPrice / 10,
		CacheWriteUSDPerMillion:  inputPrice,
		OutputUSDPerMillion:      inputPrice * 4,
		StructuredOutputs:        true,
		NoThinkLocation:          NoThinkLocationNone,
		Temperature:              &temperature,
		Seed:                     &seed,
		RequestTimeoutMS:         CampaignTaskDeadlineMS(TaskContentFilter),
	}
	if provider == "openrouter" {
		config.BaseURL = "https://openrouter.ai/api/v1"
		config.ProviderEndpoint = "test-provider/fp8"
		config.ZDR = true
	} else {
		config.BaseURL = "https://api.openai.com/v1"
	}
	return config
}

func promotionUsageForTest(
	config SystemConfig,
	requestID string,
	inputTokens int64,
	outputTokens int64,
) Usage {
	costSource := CostSourceManifestEstimate
	if config.Provider == "openrouter" {
		costSource = CostSourceProviderReported
	}
	return Usage{
		RequestID:    requestID,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CostMicroUSD: config.EstimateCostWithCacheWriteMicroUSD(
			inputTokens,
			0,
			0,
			outputTokens,
		),
		CostSource:         costSource,
		AccountingComplete: true,
	}
}

func promotionContentResults(
	corpus Corpus,
	config SystemConfig,
	manifestSHA string,
	routeSnapshotSHA string,
	_ int64,
) []ResultRecord {
	results := make([]ResultRecord, 0, len(corpus.Records))
	for index, record := range corpus.Records {
		isEnglish := record.ContentFilter.Expected.Language == LanguageEnglish
		action := ContentFilterActionDrop
		if isEnglish {
			action = ContentFilterActionKeep
		}
		requestContractSHA, err := RequestContractSHA256(
			record,
			config,
			ProductionThresholds(),
		)
		if err != nil {
			panic(err)
		}
		audit := ExecutionAudit{
			ManifestSHA256: manifestSHA,
			Route: RouteAudit{
				ReturnedModel: "test/model",
				Proof:         RouteProofDirectResponseModel,
				Verified:      true,
			},
		}
		if config.Provider == "openrouter" {
			audit.Route = RouteAudit{
				SnapshotSHA256:    routeSnapshotSHA,
				SnapshotFetchedAt: testRouteSnapshotFetched,
				ReturnedModel:     "test/model",
				ReturnedProvider:  "Test Provider",
				Proof:             RouteProofRouterMetadata,
				Verified:          true,
			}
		}
		costSource := CostSourceManifestEstimate
		if config.Provider == "openrouter" {
			costSource = CostSourceProviderReported
		}
		costMicroUSD := config.EstimateCostWithCacheWriteMicroUSD(100, 0, 0, 10)
		results = append(results, ResultRecord{
			SchemaVersion:         SchemaVersion,
			CorpusSHA256:          corpus.SHA256,
			RequestContractSHA256: requestContractSHA,
			EvaluatorBuildSHA256:  testEvaluatorBuildSHA,
			ExecutionAudit:        audit,
			CaseID:                record.CaseID,
			Task:                  record.Task,
			System:                config.Descriptor(),
			Status:                ResultStatusOK,
			ContentFilter: &ContentFilterResult{
				Action:     action,
				IsEnglish:  &isEnglish,
				Confidence: 0.99,
			},
			Usage: Usage{
				RequestID:          fmt.Sprintf("%s:request:%04d", config.SystemID, index),
				InputTokens:        100,
				OutputTokens:       10,
				CostMicroUSD:       costMicroUSD,
				CostSource:         costSource,
				AccountingComplete: true,
			},
			RequestTiming: &RequestTiming{
				ElapsedMS:  500,
				DeadlineMS: config.RequestTimeoutMS,
			},
		})
	}
	return results
}

func promotionMatcherExtractResults(
	t *testing.T,
	corpus Corpus,
	config SystemConfig,
	manifestSHA string,
	routeSnapshotSHA string,
	_ int64,
) []ResultRecord {
	t.Helper()
	results := make([]ResultRecord, 0, len(corpus.Records))
	for index, record := range corpus.Records {
		require.NotEmpty(t, record.MatcherExtract.Expected.Acceptable)
		extraction := record.MatcherExtract.Expected.Acceptable[0]
		requestContractSHA, err := RequestContractSHA256(
			record,
			config,
			ProductionThresholds(),
		)
		require.NoError(t, err)
		audit := ExecutionAudit{
			ManifestSHA256: manifestSHA,
			Route: RouteAudit{
				ReturnedModel: "test/model",
				Proof:         RouteProofDirectResponseModel,
				Verified:      true,
			},
		}
		if config.Provider == "openrouter" {
			audit.Route = RouteAudit{
				SnapshotSHA256:    routeSnapshotSHA,
				SnapshotFetchedAt: testRouteSnapshotFetched,
				ReturnedModel:     "test/model",
				ReturnedProvider:  "Test Provider",
				Proof:             RouteProofRouterMetadata,
				Verified:          true,
			}
		}
		costSource := CostSourceManifestEstimate
		if config.Provider == "openrouter" {
			costSource = CostSourceProviderReported
		}
		costMicroUSD := config.EstimateCostWithCacheWriteMicroUSD(100, 0, 0, 10)
		results = append(results, ResultRecord{
			SchemaVersion:         SchemaVersion,
			CorpusSHA256:          corpus.SHA256,
			RequestContractSHA256: requestContractSHA,
			EvaluatorBuildSHA256:  testEvaluatorBuildSHA,
			ExecutionAudit:        audit,
			CaseID:                record.CaseID,
			Task:                  record.Task,
			System:                config.Descriptor(),
			Status:                ResultStatusOK,
			MatcherExtract: &MatcherExtractResult{
				Action:     MatcherExtractActionExtract,
				Extraction: &extraction,
			},
			Usage: Usage{
				RequestID: fmt.Sprintf(
					"%s:extract-request:%04d",
					config.SystemID,
					index,
				),
				InputTokens:        100,
				OutputTokens:       10,
				CostMicroUSD:       costMicroUSD,
				CostSource:         costSource,
				AccountingComplete: true,
			},
			RequestTiming: &RequestTiming{
				ElapsedMS:  500,
				DeadlineMS: config.RequestTimeoutMS,
			},
		})
	}
	return results
}

func rebuildPromotionCampaignFixture(
	t *testing.T,
	input *PromotionBundleInput,
) {
	t.Helper()
	taskCorpus, err := FilterCorpus(input.FullHoldout, input.Task, 0)
	require.NoError(t, err)
	gate := testContentFilterCampaignGate()
	if input.Task == TaskMatcherExtract {
		gate = testMatcherExtractCampaignGate()
	} else {
		gate.CriticalStrata = []CampaignCriticalStratumGate{
			{
				SliceID:              "hard_must_keep_english",
				MinimumCases:         2_000,
				MaxSeverityOneErrors: 0,
				MaxHarmRate:          CampaignMaxAbsoluteHarmRate,
				MinUtilityRate:       0.8,
			},
			{
				SliceID:              "latin_script_non_english",
				MinimumCases:         500,
				MaxSeverityOneErrors: 0,
				MaxHarmRate:          CampaignMaxAbsoluteHarmRate,
				MinUtilityRate:       0.8,
			},
		}
	}
	// The fixture's comparison settings are preregistered into the immutable
	// campaign, mirroring the production protocol instead of supplying an
	// independent post-holdout knob.
	if input.ComparisonOptions.BootstrapReplicates != 0 {
		gate.Inference.BootstrapReplicates =
			input.ComparisonOptions.BootstrapReplicates
	}
	if input.ComparisonOptions.BootstrapSeed != 0 {
		gate.Inference.BootstrapSeed = input.ComparisonOptions.BootstrapSeed
	}
	plan := CampaignPlan{
		SchemaVersion:        CampaignSchemaVersion,
		CampaignID:           "campaign-promotion-holdout-fixture",
		BenchmarkEpoch:       "2026-07-28",
		ProtocolVersion:      CampaignProtocolVersion,
		ShadowOnly:           true,
		Stage:                CampaignStageHoldout,
		CorpusPlanID:         input.GoldClosureManifest.PlanID,
		CorpusPlanSHA256:     input.GoldClosureManifest.PlanSHA256,
		SystemManifestSHA256: input.SystemManifestSHA256,
		OutputRoot:           "/var/tmp/bitagent-promotion-fixture",
		SummaryPath:          "summary.json",
		AllowedActions: []CampaignAction{
			CampaignActionValidate,
			CampaignActionRunShadow,
			CampaignActionScore,
			CampaignActionCompare,
		},
		TotalCostCapMicroUSD: int64(len(input.Systems)) * 100_000,
		TaskArtifacts: []CampaignTaskArtifacts{{
			Task: input.Task,
			Corpus: CampaignArtifactBinding{
				ArtifactID: "promotion-content-holdout",
				SHA256:     taskCorpus.SHA256,
			},
			GoldClosure: &CampaignArtifactBinding{
				ArtifactID: "promotion-gold-closure",
				SHA256:     input.GoldClosureManifestSHA256,
			},
			PrivacySidecar: &CampaignArtifactBinding{
				ArtifactID: "promotion-privacy-sidecar",
				SHA256: input.GoldClosureManifest.
					PrivacySidecarSHA256,
			},
		}},
		HistoricalBaselines: []CampaignHistoricalBaseline{{
			BaselineID:      "promotion-historical-baseline",
			Task:            input.Task,
			SystemID:        "historical-production",
			BenchmarkEpoch:  "2026-07-27",
			ProtocolVersion: "historical-diagnostic-v1",
			Artifact: CampaignArtifactBinding{
				ArtifactID: "promotion-historical-report",
				SHA256:     strings.Repeat("d", 64),
			},
		}},
		EffectivenessGates: []CampaignEffectivenessGate{
			gate,
		},
	}
	for index, evidence := range input.Systems {
		role := CampaignSystemRoleCandidate
		controlKind := CampaignControlKind("")
		if evidence.Role == PromotionRoleControl {
			role = CampaignSystemRoleControl
			controlKind = CampaignControlKindNormalized
			for _, config := range input.SystemManifest.Systems {
				if config.SystemID == evidence.SystemID &&
					config.EvaluationLane ==
						EvaluationLaneProductionFidelity {
					controlKind = CampaignControlKindDeployed
				}
			}
		}
		plan.Systems = append(plan.Systems, CampaignSystemMatrix{
			SystemID:    evidence.SystemID,
			Role:        role,
			ControlKind: controlKind,
			Tasks:       []Task{input.Task},
		})
		if evidence.RouteSnapshotSHA256 != "" {
			plan.RouteSnapshots = append(
				plan.RouteSnapshots,
				CampaignRouteSnapshot{
					SystemID:              evidence.SystemID,
					ExactEndpointEvidence: true,
					Snapshot: CampaignArtifactBinding{
						ArtifactID: "promotion-route-" + evidence.SystemID,
						SHA256:     evidence.RouteSnapshotSHA256,
					},
				},
			)
		}
		plan.Runs = append(plan.Runs, CampaignRun{
			RunID:           "promotion-holdout-" + evidence.SystemID,
			SystemID:        evidence.SystemID,
			Task:            input.Task,
			CostCapMicroUSD: 100_000,
			Outputs: CampaignRunOutputs{
				ResultsPath: evidence.SystemID + "/results.jsonl",
				AttemptEvidencePath: evidence.SystemID +
					"/attempts.json",
				ScorePath: evidence.SystemID + "/score.json",
			},
		})
		_ = index
	}
	for candidateIndex, candidate := range input.Systems {
		if candidate.Role != PromotionRoleCandidate {
			continue
		}
		for controlIndex, control := range input.Systems {
			if control.Role != PromotionRoleControl {
				continue
			}
			plan.Comparisons = append(plan.Comparisons, CampaignComparison{
				ComparisonID: fmt.Sprintf(
					"promotion-comparison-%d-%d",
					candidateIndex,
					controlIndex,
				),
				ControlRunID: "promotion-holdout-" + control.SystemID,
				CandidateRunID: "promotion-holdout-" +
					candidate.SystemID,
				OutputPath: fmt.Sprintf(
					"comparisons/candidate-%d-control-%d.json",
					candidateIndex,
					controlIndex,
				),
			})
		}
	}
	raw, err := json.Marshal(plan)
	require.NoError(t, err)
	campaignEvidence, err := ReadPromotionCampaignPlanEvidence(
		bytes.NewReader(raw),
	)
	require.NoError(t, err)
	input.CampaignPlan = campaignEvidence
	for index := range input.Systems {
		evidence := &input.Systems[index]
		binding, err := campaignEvidence.Plan.RunBinding(
			campaignEvidence.SHA256,
			"promotion-holdout-"+evidence.SystemID,
		)
		require.NoError(t, err)
		for resultIndex := range evidence.Results {
			bound := binding
			evidence.Results[resultIndex].ExecutionAudit.Campaign = &bound
		}
		config := SystemConfig{}
		for _, candidate := range input.SystemManifest.Systems {
			if candidate.SystemID == evidence.SystemID {
				config = candidate
				break
			}
		}
		evidence.AttemptEvidence = promotionAttemptFixture(
			t,
			taskCorpus,
			config,
			evidence.Results,
		)
	}
}

func promotionAttemptFixture(
	t *testing.T,
	corpus Corpus,
	config SystemConfig,
	results []ResultRecord,
) PromotionAttemptEvidence {
	t.Helper()
	resultsSHA, err := ResultsIdentity(results)
	require.NoError(t, err)
	attempts := make([]PromotionAttempt, 0, len(results))
	for index, result := range results {
		attempts = append(attempts, PromotionAttempt{
			CaseID:   result.CaseID,
			Sequence: 1,
			Kind:     PromotionAttemptInitial,
			Status:   ResultStatusOK,
			Usage:    result.Usage,
		})
		_ = index
	}
	return PromotionAttemptEvidence{
		SchemaVersion:   SchemaVersion,
		ProtocolVersion: PromotionBundleProtocolVersion,
		SystemID:        config.SystemID,
		Task:            corpus.Records[0].Task,
		CorpusSHA256:    corpus.SHA256,
		ResultsSHA256:   resultsSHA,
		Attempts:        attempts,
	}
}

func promotionGoldClosureManifest(corpus Corpus) GoldClosureManifest {
	counts := map[Task]int{}
	for _, record := range corpus.Records {
		counts[record.Task]++
	}
	contentJunkCases := counts[TaskContentFilter] + counts[TaskJunkPurge]
	matcherCases := counts[TaskMatcherExtract] + counts[TaskMatcherRerank]
	artifact := GoldClosureReviewArtifactHashes{
		PrimaryAssignmentASHA256: strings.Repeat("a", 64),
		PrimarySubmissionASHA256: strings.Repeat("b", 64),
		PrimaryAssignmentBSHA256: strings.Repeat("c", 64),
		PrimarySubmissionBSHA256: strings.Repeat("d", 64),
	}
	review := func(workflow ReviewWorkflow, cases int) GoldClosureReviewSummary {
		return GoldClosureReviewSummary{
			Workflow:               workflow,
			ReviewSetID:            "review:fixture",
			PolicyID:               "policy:fixture",
			PolicyVersion:          "policy-v1",
			PolicySHA256:           testEvaluatorBuildSHA,
			EvidenceManifestID:     "evidence:fixture",
			EvidenceManifestSHA256: testEvaluatorBuildSHA,
			EvidenceCorpusSHA256:   testEvaluatorBuildSHA,
			SourceSnapshotSHA256:   testEvaluatorBuildSHA,
			ReviewedCases:          cases,
			PromotedGoldCases:      cases,
			Artifacts:              artifact,
		}
	}
	manifest := GoldClosureManifest{
		SchemaVersion:                    SchemaVersion,
		Status:                           GoldClosureStatus,
		Scope:                            "test",
		SelectionAlgorithmID:             GoldClosureAlgorithmID,
		PlanID:                           "plan:fixture",
		PlanSHA256:                       strings.Repeat("1", 64),
		PlanExecutable:                   true,
		ProductionSourceExporterVerified: true,
		ProductionExportManifestSHA256:   strings.Repeat("2", 64),
		PrivacySidecarSHA256:             strings.Repeat("3", 64),
		CandidateCorpusSHA256:            strings.Repeat("4", 64),
		CandidateCases:                   len(corpus.Records),
		CandidateFreezeManifestSHA256:    strings.Repeat("5", 64),
		CandidateDevelopmentSHA256:       strings.Repeat("6", 64),
		CandidateHoldoutSHA256:           strings.Repeat("7", 64),
		DevelopmentCorpusSHA256:          strings.Repeat("8", 64),
		DevelopmentCases:                 4,
		ReviewPhaseSeparated:             true,
		DevelopmentClosureManifestSHA256: strings.Repeat("a", 64),
		FinalistRosterManifestSHA256:     strings.Repeat("b", 64),
		HoldoutCorpusSHA256:              corpus.SHA256,
		HoldoutCases:                     len(corpus.Records),
		ReviewWorkflows: []GoldClosureReviewSummary{
			review(ReviewWorkflowMatcher, matcherCases),
			review(ReviewWorkflowContentJunk, contentJunkCases),
		},
	}
	for _, task := range orderedTasks {
		manifest.Tasks = append(manifest.Tasks, GoldClosureTaskSummary{
			Task:                 task,
			SourceSnapshotSHA256: testEvaluatorBuildSHA,
			DevelopmentCases:     1,
			HoldoutCases:         counts[task],
		})
	}
	return manifest
}

func requirePromotionSystemReport(
	t *testing.T,
	bundle PromotionBundle,
	systemID string,
) PromotionSystemReport {
	t.Helper()
	for _, report := range bundle.Systems {
		if report.System.SystemID == systemID {
			return report
		}
	}
	t.Fatalf("promotion system report %q not found", systemID)
	return PromotionSystemReport{}
}
