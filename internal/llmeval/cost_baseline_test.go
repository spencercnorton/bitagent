package llmeval

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

type checkedCostBaseline struct {
	ArtifactKind string `json:"artifact_kind"`
	Status       string `json:"status"`
	Observed     struct {
		Source struct {
			ScopeClassification       string `json:"scope_classification"`
			QueryIdentityPreserved    bool   `json:"query_identity_preserved"`
			ProjectOrAPIKeyPreserved  bool   `json:"project_or_api_key_filter_preserved"`
			ServiceTierSplitPreserved bool   `json:"service_tier_breakdown_preserved"`
			AttributionCaveat         string `json:"attribution_caveat"`
		} `json:"source"`
		WindowDays int `json:"window_days"`
		Workloads  []struct {
			WorkloadProxy     string  `json:"workload_proxy"`
			Requests          int64   `json:"requests"`
			InputTokens       int64   `json:"input_tokens"`
			CachedInputTokens int64   `json:"cached_input_tokens"`
			OutputTokens      int64   `json:"output_tokens"`
			BilledUSD         float64 `json:"billed_usd"`
		} `json:"workloads"`
		Totals struct {
			Requests          int64   `json:"requests"`
			InputTokens       int64   `json:"input_tokens"`
			CachedInputTokens int64   `json:"cached_input_tokens"`
			OutputTokens      int64   `json:"output_tokens"`
			BilledUSD         float64 `json:"billed_usd"`
		} `json:"totals"`
	} `json:"observed"`
	PointInTimeChecks []struct {
		QueriedAtUTC           string   `json:"queried_at_utc"`
		BucketStartUTC         string   `json:"bucket_start_utc"`
		BucketEndExclusiveUTC  string   `json:"bucket_end_exclusive_utc"`
		PartialBucketBilledUSD float64  `json:"partial_bucket_billed_usd"`
		Classification         string   `json:"classification"`
		Caveats                []string `json:"caveats"`
	} `json:"point_in_time_checks"`
	ProjectionMethod struct {
		OpenRouterCashMultiplier float64 `json:"openrouter_cash_multiplier"`
	} `json:"projection_method"`
	Projections struct {
		ObservedInvoice struct {
			TotalProjected30DayBilledUSD float64 `json:"total_projected_30_day_billed_usd"`
		} `json:"observed_openai_invoice_linear_30_day_extrapolation"`
		General struct {
			InputTokensPer7DayWindow  int64                  `json:"input_tokens_per_7_day_window"`
			OutputTokensPer7DayWindow int64                  `json:"output_tokens_per_7_day_window"`
			Systems                   []checkedProjectionRow `json:"systems"`
		} `json:"openrouter_general_generative_same_token_volume"`
		GeneralByWorkload struct {
			Systems []struct {
				SystemID  string             `json:"system_id"`
				Workloads map[string]float64 `json:"projected_30_day_cash_usd_by_workload"`
				Total     float64            `json:"projected_30_day_cash_usd_total"`
			} `json:"systems"`
		} `json:"openrouter_general_generative_by_workload_same_token_volume"`
		EvaluationBudget struct {
			Stages []struct {
				Stage                   string             `json:"stage"`
				Cases                   int64              `json:"cases"`
				CaseAllocation          map[string]int64   `json:"case_allocation"`
				EstimatedInputTokens    int64              `json:"estimated_input_tokens"`
				EstimatedOutputTokens   int64              `json:"estimated_output_tokens"`
				OpenAIControlsSyncUSD   float64            `json:"current_openai_controls_standard_sync_usd"`
				OpenAIControlsBatchUSD  float64            `json:"current_openai_controls_batch_price_equivalent_usd"`
				OpenRouterCandidateCash map[string]float64 `json:"openrouter_candidate_cash_usd"`
				AllCandidatesCashUSD    float64            `json:"all_registered_openrouter_candidates_cash_usd"`
			} `json:"stages"`
			Repeatability struct {
				Classification                   string             `json:"classification"`
				SamplingAlgorithmID              string             `json:"sampling_algorithm_id"`
				SampledGroupsPerTask             int64              `json:"sampled_groups_per_task"`
				SampledCasesPerTask              int64              `json:"sampled_cases_per_task"`
				NaturalGroupsPerTask             int64              `json:"natural_groups_per_task"`
				SafetyGroupsPerTask              int64              `json:"safety_groups_per_task"`
				RepeatRuns                       int64              `json:"repeat_runs"`
				IncrementalCasesPerSystemPerTask int64              `json:"incremental_cases_per_nondeterministic_system_per_task"`
				DualOpenAIControlsBatchUSD       float64            `json:"dual_openai_controls_batch_incremental_usd"`
				OpenRouterCandidateCash          map[string]float64 `json:"openrouter_candidate_incremental_cash_usd"`
				AllOpenRouterIncrementalCashUSD  float64            `json:"all_nondeterministic_openrouter_candidates_incremental_cash_usd"`
				MaxOpenRouterCashUSD             float64            `json:"maximum_all_stage_1_passers_openrouter_cash_usd_including_repeats"`
				MaxDualOpenAIControlsBatchUSD    float64            `json:"maximum_dual_openai_controls_batch_usd_including_repeats"`
				MaxCombinedEvaluationCashUSD     float64            `json:"maximum_combined_evaluation_cash_usd_if_every_stage_1_candidate_passes"`
				Caveat                           string             `json:"caveat"`
			} `json:"repeatability_screen"`
		} `json:"evaluation_budget_screen"`
		Specialists []checkedProjectionRow `json:"openrouter_task_specialists"`
		Excluded    []struct {
			SystemID string `json:"system_id"`
		} `json:"excluded_manifest_references"`
	} `json:"projections"`
}

type checkedProjectionRow struct {
	SystemID                        string  `json:"system_id"`
	InputTokensPer7DayWindow        int64   `json:"input_tokens_per_7_day_window"`
	OutputTokensPer7DayWindow       int64   `json:"output_tokens_per_7_day_window"`
	InputUSDPerMillion              float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion             float64 `json:"output_usd_per_million"`
	WeeklyProviderCreditUSD         float64 `json:"weekly_provider_credit_usd"`
	Projected30DayProviderCreditUSD float64 `json:"projected_30_day_provider_credit_usd"`
	Projected30DayCashUSD           float64 `json:"projected_30_day_cash_usd"`
}

func TestCheckedInCostBaselineReconcilesAndUsesManifestPrices(t *testing.T) {
	raw, err := os.ReadFile(opsFixture(
		t,
		"../../ops/llm-eval/observed-cost-baseline-2026-07-17-to-2026-07-24.json",
	))
	require.NoError(t, err)

	var baseline checkedCostBaseline
	require.NoError(t, json.Unmarshal(raw, &baseline))
	require.Equal(
		t,
		"organization_model_attributed_cost_upper_bound_and_fixed_volume_price_projection",
		baseline.ArtifactKind,
	)
	require.Equal(
		t,
		"organization_model_attributed_upper_bound_with_hypothetical_estimates",
		baseline.Status,
	)
	require.Equal(
		t,
		"organization_wide_model_attributed_upper_bound",
		baseline.Observed.Source.ScopeClassification,
	)
	require.False(t, baseline.Observed.Source.QueryIdentityPreserved)
	require.False(t, baseline.Observed.Source.ProjectOrAPIKeyPreserved)
	require.False(t, baseline.Observed.Source.ServiceTierSplitPreserved)
	require.NotEmpty(t, baseline.Observed.Source.AttributionCaveat)
	require.Equal(t, 7, baseline.Observed.WindowDays)
	require.Len(t, baseline.Observed.Workloads, 3)

	var requests, inputTokens, cachedInputTokens, outputTokens int64
	var billedUSD float64
	for _, workload := range baseline.Observed.Workloads {
		requests += workload.Requests
		inputTokens += workload.InputTokens
		cachedInputTokens += workload.CachedInputTokens
		outputTokens += workload.OutputTokens
		billedUSD += workload.BilledUSD
	}
	require.Equal(t, baseline.Observed.Totals.Requests, requests)
	require.Equal(t, baseline.Observed.Totals.InputTokens, inputTokens)
	require.Equal(t, baseline.Observed.Totals.CachedInputTokens, cachedInputTokens)
	require.Equal(t, baseline.Observed.Totals.OutputTokens, outputTokens)
	require.InDelta(t, baseline.Observed.Totals.BilledUSD, billedUSD, 0.0000000001)
	require.EqualValues(t, 102583, requests)
	require.EqualValues(t, 64582868, inputTokens)
	require.Zero(t, cachedInputTokens)
	require.EqualValues(t, 4031263, outputTokens)
	require.InDelta(t, 28.09532545, billedUSD, 0.0000000001)
	require.InDelta(
		t,
		roundCostNine(billedUSD*30/7),
		baseline.Projections.ObservedInvoice.TotalProjected30DayBilledUSD,
		0.0000000001,
	)
	require.Len(t, baseline.PointInTimeChecks, 1)
	pointCheck := baseline.PointInTimeChecks[0]
	require.Equal(t, "2026-07-24T16:25:28Z", pointCheck.QueriedAtUTC)
	require.Equal(t, "2026-07-24T00:00:00Z", pointCheck.BucketStartUTC)
	require.Equal(t, "2026-07-25T00:00:00Z", pointCheck.BucketEndExclusiveUTC)
	require.InDelta(t, 0.05537725, pointCheck.PartialBucketBilledUSD, 0.0000000001)
	require.Equal(t, "mutable_partial_day_observation_not_a_run_rate", pointCheck.Classification)
	require.NotEmpty(t, pointCheck.Caveats)

	manifest, err := LoadSystemManifest(opsFixture(t, "../../ops/llm-eval/first-wave-models.json"))
	require.NoError(t, err)
	manifestSystems := make(map[string]SystemConfig, len(manifest.Systems))
	normalizedOpenRouterSystems := make(map[string]SystemConfig)
	for _, system := range manifest.Systems {
		manifestSystems[system.SystemID] = system
		if system.Provider == "openrouter" &&
			system.EvaluationLane == EvaluationLaneNormalizedStrict {
			normalizedOpenRouterSystems[system.SystemID] = system
		}
	}

	require.EqualValues(
		t,
		inputTokens,
		baseline.Projections.General.InputTokensPer7DayWindow,
	)
	require.EqualValues(
		t,
		outputTokens,
		baseline.Projections.General.OutputTokensPer7DayWindow,
	)
	require.InDelta(t, 1.055, baseline.ProjectionMethod.OpenRouterCashMultiplier, 0)

	accountedOpenRouterSystems := make(map[string]bool)
	for _, row := range baseline.Projections.General.Systems {
		checkProjectionRow(
			t,
			row,
			baseline.Projections.General.InputTokensPer7DayWindow,
			baseline.Projections.General.OutputTokensPer7DayWindow,
			baseline.ProjectionMethod.OpenRouterCashMultiplier,
			manifestSystems,
		)
		accountedOpenRouterSystems[row.SystemID] = true
	}
	for _, row := range baseline.Projections.Specialists {
		checkProjectionRow(
			t,
			row,
			row.InputTokensPer7DayWindow,
			row.OutputTokensPer7DayWindow,
			baseline.ProjectionMethod.OpenRouterCashMultiplier,
			manifestSystems,
		)
		accountedOpenRouterSystems[row.SystemID] = true
	}
	for _, row := range baseline.Projections.Excluded {
		require.False(t, accountedOpenRouterSystems[row.SystemID], row.SystemID)
		accountedOpenRouterSystems[row.SystemID] = true
	}
	for _, system := range manifest.Systems {
		if system.Provider == "openrouter" {
			require.True(t, accountedOpenRouterSystems[system.SystemID], system.SystemID)
		}
	}

	workloadKeys := []string{
		"matcher_extract_and_rerank_combined",
		"contentfilter",
		"junkpurge",
	}
	workloads := make(map[string]struct {
		InputTokens  int64
		OutputTokens int64
	}, len(baseline.Observed.Workloads))
	for _, workload := range baseline.Observed.Workloads {
		workloads[workload.WorkloadProxy] = struct {
			InputTokens  int64
			OutputTokens int64
		}{
			InputTokens:  workload.InputTokens,
			OutputTokens: workload.OutputTokens,
		}
	}
	require.Len(t, baseline.Projections.GeneralByWorkload.Systems, len(baseline.Projections.General.Systems))
	generalTotals := make(map[string]float64, len(baseline.Projections.General.Systems))
	for _, row := range baseline.Projections.General.Systems {
		generalTotals[row.SystemID] = row.Projected30DayCashUSD
	}
	for _, row := range baseline.Projections.GeneralByWorkload.Systems {
		system, exists := manifestSystems[row.SystemID]
		require.True(t, exists, row.SystemID)
		require.Len(t, row.Workloads, len(workloadKeys), row.SystemID)
		var total float64
		for _, key := range workloadKeys {
			workload, exists := workloads[key]
			require.True(t, exists, key)
			weeklyCredits := float64(workload.InputTokens)/1_000_000*system.InputUSDPerMillion +
				float64(workload.OutputTokens)/1_000_000*system.OutputUSDPerMillion
			wantCash := roundCostNine(weeklyCredits * 30 / 7 * baseline.ProjectionMethod.OpenRouterCashMultiplier)
			// Artifact values use exact decimal arithmetic; float64 can land
			// one 1e-9 display unit either side of the half-up boundary.
			require.InDelta(t, wantCash, row.Workloads[key], 0.0000000011, row.SystemID+" "+key)
			total += row.Workloads[key]
		}
		// The three displayed workload components are each rounded to nine
		// decimals, so their displayed sum may differ from the independently
		// rounded all-volume total by up to 1.5e-9.
		require.InDelta(t, roundCostNine(total), row.Total, 0.000000002, row.SystemID)
		require.InDelta(t, generalTotals[row.SystemID], row.Total, 0.000000001, row.SystemID)
	}

	require.Len(t, baseline.Projections.EvaluationBudget.Stages, 3)
	for _, stage := range baseline.Projections.EvaluationBudget.Stages {
		var cases int64
		var estimatedInputTokens, estimatedOutputTokens, controlCost float64
		for _, count := range stage.CaseAllocation {
			cases += count
		}
		require.Equal(t, stage.Cases, cases, stage.Stage)
		for taskName, count := range stage.CaseAllocation {
			task := Task(taskName)
			workloadKey := taskName
			if task == TaskMatcherExtract || task == TaskMatcherRerank {
				workloadKey = "matcher_extract_and_rerank_combined"
			}
			workload, exists := workloads[workloadKey]
			require.True(t, exists, taskName)
			var requests int64
			for _, candidate := range baseline.Observed.Workloads {
				if candidate.WorkloadProxy == workloadKey {
					requests = candidate.Requests
					break
				}
			}
			require.Greater(t, requests, int64(0), taskName)
			inputTokens := float64(workload.InputTokens) / float64(requests) * float64(count)
			outputTokens := float64(workload.OutputTokens) / float64(requests) * float64(count)
			estimatedInputTokens += inputTokens
			estimatedOutputTokens += outputTokens

			controlCount := 0
			for _, system := range manifest.Systems {
				if system.Provider == "openai" &&
					(system.EvaluationLane == EvaluationLaneProductionFidelity ||
						system.EvaluationLane == EvaluationLaneNormalizedStrict) &&
					system.SupportsTask(task) {
					controlCount++
					controlCost += inputTokens/1_000_000*
						system.InputUSDPerMillion +
						outputTokens/1_000_000*
							system.OutputUSDPerMillion
				}
			}
			require.Equal(
				t,
				2,
				controlCount,
				"task %s must have deployed and normalized controls",
				task,
			)
		}
		require.Equal(t, int64(math.Round(estimatedInputTokens)), stage.EstimatedInputTokens, stage.Stage)
		require.Equal(t, int64(math.Round(estimatedOutputTokens)), stage.EstimatedOutputTokens, stage.Stage)
		// The artifact is rounded from exact decimal arithmetic; binary
		// float accumulation can land one 1e-9 display unit across the
		// half-up boundary.
		require.InDelta(
			t,
			roundCostNine(controlCost),
			stage.OpenAIControlsSyncUSD,
			0.0000000011,
			stage.Stage,
		)
		if stage.OpenAIControlsBatchUSD != 0 {
			require.InDelta(
				t,
				stage.OpenAIControlsSyncUSD/2,
				stage.OpenAIControlsBatchUSD,
				0.000000001,
				stage.Stage,
			)
		}
		var candidateTotal float64
		require.Len(
			t,
			stage.OpenRouterCandidateCash,
			len(normalizedOpenRouterSystems),
			stage.Stage,
		)
		for systemID := range normalizedOpenRouterSystems {
			_, exists := stage.OpenRouterCandidateCash[systemID]
			require.True(t, exists, systemID+" "+stage.Stage)
		}
		for systemID, cashUSD := range stage.OpenRouterCandidateCash {
			system, exists := normalizedOpenRouterSystems[systemID]
			require.True(t, exists, systemID)
			require.Equal(t, "openrouter", system.Provider)
			var systemInputTokens, systemOutputTokens float64
			for taskName, count := range stage.CaseAllocation {
				task := Task(taskName)
				if !system.SupportsTask(task) {
					continue
				}
				workloadKey := taskName
				if task == TaskMatcherExtract ||
					task == TaskMatcherRerank {
					workloadKey =
						"matcher_extract_and_rerank_combined"
				}
				workload := workloads[workloadKey]
				var workloadRequests int64
				for _, candidate := range baseline.Observed.Workloads {
					if candidate.WorkloadProxy == workloadKey {
						workloadRequests = candidate.Requests
						break
					}
				}
				require.Greater(t, workloadRequests, int64(0))
				systemInputTokens += float64(workload.InputTokens) /
					float64(workloadRequests) * float64(count)
				systemOutputTokens += float64(workload.OutputTokens) /
					float64(workloadRequests) * float64(count)
			}
			wantCash := (systemInputTokens/1_000_000*
				system.InputUSDPerMillion +
				systemOutputTokens/1_000_000*
					system.OutputUSDPerMillion) *
				baseline.ProjectionMethod.OpenRouterCashMultiplier
			require.InDelta(t, roundCostNine(wantCash), cashUSD, 0.0000000001, systemID+" "+stage.Stage)
			candidateTotal += cashUSD
		}
		if stage.AllCandidatesCashUSD != 0 {
			displayedSumTolerance := float64(len(stage.OpenRouterCandidateCash))*0.5e-9 + 0.1e-9
			require.InDelta(
				t,
				roundCostNine(candidateTotal),
				stage.AllCandidatesCashUSD,
				displayedSumTolerance,
				stage.Stage,
			)
		}
	}

	repeat := baseline.Projections.EvaluationBudget.Repeatability
	require.Equal(
		t,
		"worst_case_if_every_stage_1_passer_reaches_final_holdout",
		repeat.Classification,
	)
	require.Equal(
		t,
		PromotionRepeatSamplingAlgorithmID,
		repeat.SamplingAlgorithmID,
	)
	require.EqualValues(t, PromotionRepeatNaturalGroups, repeat.NaturalGroupsPerTask)
	require.EqualValues(t, PromotionRepeatSafetyGroups, repeat.SafetyGroupsPerTask)
	require.EqualValues(
		t,
		PromotionRepeatNaturalGroups+PromotionRepeatSafetyGroups,
		repeat.SampledGroupsPerTask,
	)
	require.Equal(t, repeat.SampledGroupsPerTask, repeat.SampledCasesPerTask)
	require.EqualValues(t, PromotionRequiredRepeatRuns, repeat.RepeatRuns)
	require.Equal(
		t,
		repeat.SampledCasesPerTask*repeat.RepeatRuns,
		repeat.IncrementalCasesPerSystemPerTask,
	)
	require.NotEmpty(t, repeat.Caveat)

	stageByID := make(map[string]struct {
		OpenAIControlsSyncUSD   float64
		OpenAIControlsBatchUSD  float64
		OpenRouterCandidateCash map[string]float64
		AllCandidatesCashUSD    float64
	})
	for _, stage := range baseline.Projections.EvaluationBudget.Stages {
		stageByID[stage.Stage] = struct {
			OpenAIControlsSyncUSD   float64
			OpenAIControlsBatchUSD  float64
			OpenRouterCandidateCash map[string]float64
			AllCandidatesCashUSD    float64
		}{
			OpenAIControlsSyncUSD:   stage.OpenAIControlsSyncUSD,
			OpenAIControlsBatchUSD:  stage.OpenAIControlsBatchUSD,
			OpenRouterCandidateCash: stage.OpenRouterCandidateCash,
			AllCandidatesCashUSD:    stage.AllCandidatesCashUSD,
		}
	}
	stageOne := stageByID["stage_1_development"]
	full := stageByID["development_plus_holdout"]
	require.InDelta(
		t,
		stageOne.OpenAIControlsSyncUSD,
		repeat.DualOpenAIControlsBatchUSD,
		0.000000001,
	)
	var openRouterRepeatTotal float64
	for systemID, system := range normalizedOpenRouterSystems {
		_, listed := repeat.OpenRouterCandidateCash[systemID]
		nondeterministic := system.Temperature == nil || system.Seed == nil
		require.Equal(t, nondeterministic, listed, systemID)
		if !listed {
			continue
		}
		want := stageOne.OpenRouterCandidateCash[systemID] *
			float64(PromotionRequiredRepeatRuns)
		require.InDelta(
			t,
			roundCostNine(want),
			repeat.OpenRouterCandidateCash[systemID],
			0.000000001,
			systemID,
		)
		openRouterRepeatTotal += repeat.OpenRouterCandidateCash[systemID]
	}
	require.InDelta(
		t,
		roundCostNine(openRouterRepeatTotal),
		repeat.AllOpenRouterIncrementalCashUSD,
		0.000000001,
	)
	require.InDelta(
		t,
		roundCostNine(
			full.AllCandidatesCashUSD+
				repeat.AllOpenRouterIncrementalCashUSD,
		),
		repeat.MaxOpenRouterCashUSD,
		0.000000001,
	)
	require.InDelta(
		t,
		roundCostNine(
			full.OpenAIControlsBatchUSD+
				repeat.DualOpenAIControlsBatchUSD,
		),
		repeat.MaxDualOpenAIControlsBatchUSD,
		0.000000001,
	)
	require.InDelta(
		t,
		roundCostNine(
			repeat.MaxOpenRouterCashUSD+
				repeat.MaxDualOpenAIControlsBatchUSD,
		),
		repeat.MaxCombinedEvaluationCashUSD,
		0.000000001,
	)
}

func checkProjectionRow(
	t *testing.T,
	row checkedProjectionRow,
	inputTokens int64,
	outputTokens int64,
	cashMultiplier float64,
	manifestSystems map[string]SystemConfig,
) {
	t.Helper()
	system, ok := manifestSystems[row.SystemID]
	require.True(t, ok, row.SystemID)
	require.Equal(t, "openrouter", system.Provider)
	require.InDelta(t, system.InputUSDPerMillion, row.InputUSDPerMillion, 0)
	require.InDelta(t, system.OutputUSDPerMillion, row.OutputUSDPerMillion, 0)

	weeklyCredits := float64(inputTokens)/1_000_000*row.InputUSDPerMillion +
		float64(outputTokens)/1_000_000*row.OutputUSDPerMillion
	monthlyCredits := weeklyCredits * 30 / 7
	monthlyCash := monthlyCredits * cashMultiplier
	require.InDelta(t, roundCostNine(weeklyCredits), row.WeeklyProviderCreditUSD, 0.0000000001)
	require.InDelta(
		t,
		roundCostNine(monthlyCredits),
		row.Projected30DayProviderCreditUSD,
		0.0000000001,
	)
	require.InDelta(t, roundCostNine(monthlyCash), row.Projected30DayCashUSD, 0.0000000001)
}

func roundCostNine(value float64) float64 {
	return math.Round(value*1_000_000_000) / 1_000_000_000
}
